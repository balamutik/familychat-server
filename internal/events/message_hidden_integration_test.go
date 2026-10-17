package events

import (
	"context"
	"encoding/json"
	"familychat/server/internal/database"
	"os"
	"testing"
)

func TestMessageHiddenIsPrivateDuringDispatchAndReplay(t *testing.T) {
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
	if err = database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(db)
	users := make([]string, 2)
	sessions := make([]string, 2)
	for i := range users {
		if err = db.QueryRow(ctx, `INSERT INTO users(login,password_hash,role) VALUES(left(replace(gen_random_uuid()::text,'-',''),24),'hash','user') RETURNING id::text`).Scan(&users[i]); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRow(ctx, `INSERT INTO sessions(user_id,token_hash,expires_at) VALUES($1,decode(replace(gen_random_uuid()::text,'-',''),'hex'),now()+interval '1 hour') RETURNING id::text`, users[i]).Scan(&sessions[i]); err != nil {
			t.Fatal(err)
		}
	}
	var chat string
	if err = db.QueryRow(ctx, `INSERT INTO chats(kind,title) VALUES('group','private hide') RETURNING id::text`).Scan(&chat); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(ctx, `DELETE FROM chats WHERE id=$1`, chat)
	clients := make([]*Client, 2)
	for i := range clients {
		if _, err = db.Exec(ctx, `INSERT INTO chat_members(chat_id,user_id,role) VALUES($1,$2,'member')`, chat, users[i]); err != nil {
			t.Fatal(err)
		}
		clients[i] = &Client{UserID: users[i], SessionID: sessions[i], send: make(chan []byte, 5), ctx: ctx}
		hub.clients[clients[i]] = struct{}{}
	}
	data, _ := json.Marshal(map[string]any{"user_id": users[0], "message_id": "00000000-0000-4000-8000-000000000001", "seq": 1})
	ev := Event{ChatID: chat, Type: "message_hidden", Data: data}
	if err = db.QueryRow(ctx, `INSERT INTO events(chat_id,kind,payload) VALUES($1,$2,$3) RETURNING id`, chat, ev.Type, data).Scan(&ev.ID); err != nil {
		t.Fatal(err)
	}
	hub.dispatch(ctx, ev)
	if len(clients[0].send) != 1 || len(clients[1].send) != 0 {
		t.Fatal("hidden event leaked during live dispatch")
	}
	<-clients[0].send
	for i, c := range clients {
		if err = hub.Replay(ctx, c, ev.ID-1); err != nil {
			t.Fatal(err)
		}
		if got := len(c.send); got != 1-i {
			t.Fatalf("replay user %d received %d events", i, got)
		}
	}
}
