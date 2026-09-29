package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/events"
	"github.com/coder/websocket"
)

func TestWebSocketDeliversCommittedMessageOnlyToMember(t *testing.T) {
	pool := testDatabase(t)
	_, at := seedSession(t, pool, "user")
	b, bt := seedSession(t, pool, "user")
	_, outsider := seedSession(t, pool, "admin")
	hub := events.NewHub(pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hub.Run(ctx) }()
	select {
	case <-hub.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("hub not ready")
	}
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, Events: hub})
	srv := httptest.NewServer(h)
	defer srv.Close()
	chatResponse := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, b), at)
	if chatResponse.Code != 201 {
		t.Fatalf("chat: %d", chatResponse.Code)
	}
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(chatResponse.Body.Bytes(), &chat)
	wsTicket := func(token string) string {
		r := callAPI(h, "POST", "/api/v1/auth/websocket-ticket", "", token)
		if r.Code != 201 {
			t.Fatalf("ticket: %d %s", r.Code, r.Body.String())
		}
		var v struct {
			Ticket string `json:"ticket"`
		}
		_ = json.Unmarshal(r.Body.Bytes(), &v)
		return v.Ticket
	}
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws?ticket="
	_, resp, err := websocket.Dial(context.Background(), base+wsTicket(bt), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}}})
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("foreign Origin accepted: status=%v err=%v", resp, err)
	}
	connection, _, err := websocket.Dial(context.Background(), base+wsTicket(bt), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	other, _, err := websocket.Dial(context.Background(), base+wsTicket(outsider), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.CloseNow()
	message := `{"client_message_id":"00000000-0000-4000-8000-000000000004","text":"Only members"}`
	if r := callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/messages", message, at); r.Code != 201 {
		t.Fatalf("send: %d %s", r.Code, r.Body.String())
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer readCancel()
	_, payload, err := connection.Read(readCtx)
	if err != nil || !strings.Contains(string(payload), `"type":"message"`) || !strings.Contains(string(payload), chat.ID) {
		t.Fatalf("member event=%q err=%v", payload, err)
	}
	otherCtx, otherCancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer otherCancel()
	_, payload, err = other.Read(otherCtx)
	if err == nil {
		t.Fatalf("outsider received event: %s", payload)
	}
	if r := callAPI(h, "POST", "/api/v1/auth/logout", "", bt); r.Code != 204 {
		t.Fatalf("logout: %d", r.Code)
	}
	closedCtx, closedCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closedCancel()
	_, _, err = connection.Read(closedCtx)
	if err == nil || closedCtx.Err() != nil {
		t.Fatalf("WebSocket did not close promptly after logout: err=%v ctx=%v", err, closedCtx.Err())
	}
}

func TestWebSocketDeliversLateCommittedEvent(t *testing.T) {
	pool := testDatabase(t)
	_, token := seedSession(t, pool, "user")
	hub := events.NewHub(pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hub.Run(ctx) }()
	<-hub.Ready()
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, Events: hub})
	srv := httptest.NewServer(h)
	defer srv.Close()
	r := callAPI(h, "POST", "/api/v1/chats", `{"title":"Late commits"}`, token)
	if r.Code != 201 {
		t.Fatal(r.Code)
	}
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	r = callAPI(h, "POST", "/api/v1/auth/websocket-ticket", "", token)
	if r.Code != 201 {
		t.Fatal(r.Code)
	}
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &ticket)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/ws?ticket="+ticket.Ticket, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	time.Sleep(50 * time.Millisecond)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO events(chat_id,kind) VALUES($1,'late_a')`, chat.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events(chat_id,kind) VALUES($1,'early_b')`, chat.ID); err != nil {
		t.Fatal(err)
	}
	readCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	_, payload, err := conn.Read(readCtx)
	if err != nil || !strings.Contains(string(payload), `"type":"early_b"`) {
		t.Fatalf("first event=%s err=%v", payload, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	lateCtx, lateStop := context.WithTimeout(ctx, 3*time.Second)
	defer lateStop()
	_, payload, err = conn.Read(lateCtx)
	if err != nil || !strings.Contains(string(payload), `"type":"late_a"`) {
		t.Fatalf("late committed event=%s err=%v", payload, err)
	}
}
