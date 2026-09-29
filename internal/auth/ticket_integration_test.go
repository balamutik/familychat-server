package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"familychat/server/internal/database"
)

func TestWebSocketTicketSingleUse(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO users(login,password_hash) VALUES('ticket_test_user','x') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, id) }()
	token, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := pool.QueryRow(ctx, `INSERT INTO sessions(user_id,token_hash,expires_at) VALUES($1,$2,now()+interval '1 hour') RETURNING id::text`, id, hash).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: pool, TTL: time.Hour}
	p, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := s.CreateWebSocketTicket(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ConsumeWebSocketTicket(ctx, ticket)
	if err != nil || first.UserID != id || first.SessionID != sessionID {
		t.Fatalf("first consume=%+v err=%v", first, err)
	}
	if _, err := s.ConsumeWebSocketTicket(ctx, ticket); err == nil {
		t.Fatal("ticket reused")
	}
}
