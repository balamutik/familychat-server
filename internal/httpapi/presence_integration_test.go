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

func TestPeerPresenceTracksConnectionsAndPersistsLastSeen(t *testing.T) {
	pool := testDatabase(t)
	_, viewer := seedSession(t, pool, "user")
	peerID, peer := seedSession(t, pool, "user")
	_, stranger := seedSession(t, pool, "user")
	hub := events.NewHub(pool)
	d := Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, Events: hub}
	h := New(d)
	srv := httptest.NewServer(h)
	defer srv.Close()
	created := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, peerID), viewer)
	var chat struct {
		ID string `json:"id"`
	}
	if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &chat) != nil {
		t.Fatalf("create: %s", created.Body)
	}
	path := "/api/v1/chats/" + chat.ID
	type presence struct {
		Online   bool       `json:"online"`
		LastSeen *time.Time `json:"last_seen_at"`
	}
	read := func() *presence {
		t.Helper()
		r := callAPI(h, "GET", path, "", viewer)
		var result struct {
			Presence *presence `json:"presence"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil || result.Presence == nil {
			t.Fatalf("missing presence: %s", r.Body)
		}
		return result.Presence
	}
	wait := func(predicate func(*presence) bool) *presence {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			p := read()
			if predicate(p) {
				return p
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("presence did not converge")
		return nil
	}
	initial := read()
	if initial.Online || initial.LastSeen != nil {
		t.Fatalf("new user presence=%+v", initial)
	}
	dial := func() *websocket.Conn {
		t.Helper()
		r := callAPI(h, "POST", "/api/v1/auth/websocket-ticket", "", peer)
		var ticket struct {
			Ticket string `json:"ticket"`
		}
		if json.Unmarshal(r.Body.Bytes(), &ticket) != nil {
			t.Fatal("invalid ticket")
		}
		conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/ws?ticket="+ticket.Ticket, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		return conn
	}
	first := dial()
	online := wait(func(p *presence) bool { return p.Online && p.LastSeen != nil })
	second := dial()
	online = wait(func(p *presence) bool { return p.Online && p.LastSeen != nil && p.LastSeen.After(*online.LastSeen) })
	first.CloseNow()
	stillOnline := wait(func(p *presence) bool { return p.Online && p.LastSeen != nil && p.LastSeen.After(*online.LastSeen) })
	second.CloseNow()
	offline := wait(func(p *presence) bool {
		return !p.Online && p.LastSeen != nil && p.LastSeen.After(*stillOnline.LastSeen)
	})
	list := callAPI(h, "GET", "/api/v1/chats", "", viewer)
	var page struct {
		Chats []struct {
			ID       string    `json:"id"`
			Presence *presence `json:"presence"`
		} `json:"chats"`
	}
	if json.Unmarshal(list.Body.Bytes(), &page) != nil || len(page.Chats) != 1 || page.Chats[0].Presence == nil || page.Chats[0].Presence.Online {
		t.Fatalf("list presence: %s", list.Body)
	}
	if r := callAPI(h, "GET", path, "", stranger); r.Code != 404 {
		t.Fatalf("stranger status=%d", r.Code)
	}
	// A new hub models an API restart: nobody is online, last seen is durable.
	d.Events = events.NewHub(pool)
	h = New(d)
	persisted := read()
	if persisted.Online || persisted.LastSeen == nil || !persisted.LastSeen.Equal(*offline.LastSeen) {
		t.Fatalf("restart: %+v", persisted)
	}
	group := callAPI(h, "POST", "/api/v1/chats", `{"title":"Group"}`, viewer)
	if json.Unmarshal(group.Body.Bytes(), &chat) != nil {
		t.Fatal("invalid group")
	}
	result := callAPI(h, "GET", "/api/v1/chats/"+chat.ID, "", viewer)
	var groupBody map[string]json.RawMessage
	if json.Unmarshal(result.Body.Bytes(), &groupBody) != nil {
		t.Fatal("invalid group body")
	}
	if _, exists := groupBody["presence"]; exists {
		t.Fatal("group must not expose one user's presence")
	}
}
