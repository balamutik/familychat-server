package auth

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"familychat/server/internal/database"
)

func TestConcurrentPasswordChangeHasOneWinner(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	hash, err := HashPassword("initial long password")
	if err != nil {
		t.Fatal(err)
	}
	var id string
	err = db.QueryRow(ctx, `INSERT INTO users(login,password_hash) VALUES('pwchg'||substring(replace(gen_random_uuid()::text,'-','') from 1 for 16),$1) RETURNING id::text`, hash).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Exec(ctx, `DELETE FROM users WHERE id=$1`, id)
	service := &Service{DB: db, TTL: time.Hour}
	p := Principal{UserID: id}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, next := range []string{"first new password", "second new password"} {
		wg.Add(1)
		go func(value string) {
			defer wg.Done()
			results <- service.ChangePassword(ctx, p, "initial long password", value)
		}(next)
	}
	wg.Wait()
	close(results)
	success, invalid := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrInvalidCredentials) {
			invalid++
		} else {
			t.Errorf("unexpected: %v", err)
		}
	}
	if success != 1 || invalid != 1 {
		t.Fatalf("success=%d invalid=%d", success, invalid)
	}
}
