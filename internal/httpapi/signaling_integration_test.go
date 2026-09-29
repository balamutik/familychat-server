package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/events"
	"github.com/coder/websocket"
)

func TestCallSignalOnlySelectedSessions(t *testing.T) {
	pool := testDatabase(t)
	_, caller := seedSession(t, pool, "user")
	calleeID, callee := seedSession(t, pool, "user")
	_, outsider := seedSession(t, pool, "admin")
	hub := events.NewHub(pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hub.Run(ctx) }()
	<-hub.Ready()
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, Events: hub, TurnURL: "turn:localhost:3478", TurnSecret: "test"})
	srv := httptest.NewServer(h)
	defer srv.Close()
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, calleeID), caller)
	if r.Code != 201 {
		t.Fatal(r.Code)
	}
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	r = callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/calls", `{"kind":"video"}`, caller)
	if r.Code != 201 {
		t.Fatalf("start %d", r.Code)
	}
	var call struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &call)
	r = callAPI(h, "POST", "/api/v1/calls/"+call.ID+"/accept", "", callee)
	if r.Code != 200 {
		t.Fatalf("accept %d", r.Code)
	}
	open := func(token string) *websocket.Conn {
		t.Helper()
		rr := callAPI(h, "POST", "/api/v1/auth/websocket-ticket", "", token)
		if rr.Code != 201 {
			t.Fatalf("ticket %d", rr.Code)
		}
		var ticket struct {
			Ticket string `json:"ticket"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &ticket)
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/ws?ticket="+ticket.Ticket, nil)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	from := open(caller)
	defer from.CloseNow()
	to := open(callee)
	defer to.CloseNow()
	bad := open(outsider)
	defer bad.CloseNow()
	// Ensure each connection has been attached before sending signals.
	time.Sleep(50 * time.Millisecond)
	if err := from.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"offer","call_id":%q,"data":{"sdp":"private"}}`, call.ID))); err != nil {
		t.Fatal(err)
	}
	readCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	_, payload, err := to.Read(readCtx)
	if err != nil || !strings.Contains(string(payload), `"sdp":"private"`) {
		t.Fatalf("signal=%s err=%v", payload, err)
	}
	if err := bad.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"ice","call_id":%q,"data":{"candidate":"leak"}}`, call.ID))); err != nil {
		t.Fatal(err)
	}
	badCtx, badStop := context.WithTimeout(ctx, 2*time.Second)
	defer badStop()
	_, _, err = bad.Read(badCtx)
	if err == nil || badCtx.Err() != nil {
		t.Fatalf("outsider signal socket not rejected: %v", err)
	}
}
