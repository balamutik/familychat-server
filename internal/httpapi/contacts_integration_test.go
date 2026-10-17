package httpapi

import (
	"context"
	"encoding/json"
	"familychat/server/internal/auth"
	"familychat/server/internal/config"
	"familychat/server/internal/contentcrypto"
	"familychat/server/internal/events"
	"familychat/server/internal/objects"
	"fmt"
	"github.com/coder/websocket"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestContactBlockingEnforcesBothDirectionsWithoutDeletingHistory(t *testing.T) {
	db := testDatabase(t)
	aid, a := seedSession(t, db, "user")
	bid, b := seedSession(t, db, "user")
	cid, outsider := seedSession(t, db, "user")
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, Objects: objects.New(config.Config{})})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, bid), a)
	var chat chatView
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	base := "/api/v1/chats/" + chat.ID
	if _, err := db.Exec(t.Context(), `UPDATE users SET display_name='Peer Name',avatar_key='peer-avatar-'||id::text,last_seen_at=now() WHERE id=$1`, bid); err != nil {
		t.Fatal(err)
	}
	type contact struct {
		ID          string        `json:"id"`
		Login       string        `json:"login"`
		DisplayName string        `json:"display_name"`
		AvatarURL   string        `json:"avatar_url"`
		Presence    *peerPresence `json:"presence"`
		Blocked     bool          `json:"blocked_by_me"`
		CanMessage  bool          `json:"can_message"`
		CanCall     bool          `json:"can_call"`
	}
	card := func(token string) contact {
		t.Helper()
		r := callAPI(h, "GET", base+"/contact", "", token)
		var v contact
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &v) != nil {
			t.Fatalf("card: %d %s", r.Code, r.Body.String())
		}
		return v
	}
	initial := card(a)
	if initial.ID != bid || initial.DisplayName != "Peer Name" || initial.AvatarURL != avatarURL(bid) || initial.Presence == nil || !initial.CanMessage || !initial.CanCall || initial.Blocked {
		t.Fatalf("profile %+v", initial)
	}
	for _, token := range []string{"", outsider} {
		r = callAPI(h, "GET", base+"/contact", "", token)
		want := 404
		if token == "" {
			want = 401
		}
		if r.Code != want {
			t.Fatalf("card privacy: %d", r.Code)
		}
	}
	body := `{"client_message_id":"00000000-0000-4000-8000-000000000051","text":"history survives"}`
	r = callAPI(h, "POST", base+"/messages", body, a)
	var message messageView
	_ = json.Unmarshal(r.Body.Bytes(), &message)
	set := func(target, token string, blocked bool) {
		t.Helper()
		r := callAPI(h, "PUT", "/api/v1/users/"+target+"/block", fmt.Sprintf(`{"blocked":%t}`, blocked), token)
		if r.Code != 204 {
			t.Fatalf("block: %d %s", r.Code, r.Body.String())
		}
	}
	var fileID string
	if err := db.QueryRow(t.Context(), `INSERT INTO attachments(chat_id,uploader_id,object_key,filename,content_type,size_bytes,preview_state,encrypted) VALUES($1,$2,gen_random_uuid()::text,'photo.jpg','image/jpeg',12,'unsupported',true) RETURNING id::text`, chat.ID, aid).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `INSERT INTO message_attachments(message_id,attachment_id) VALUES($1,$2)`, message.ID, fileID); err != nil {
		t.Fatal(err)
	}
	set(bid, a, true)
	set(bid, a, true)
	var updates int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE chat_id=$1 AND kind='contact_updated' AND payload->>'user_id'=$2`, chat.ID, aid).Scan(&updates); err != nil || updates != 1 {
		t.Fatalf("contact event idempotence: %d %v", updates, err)
	}
	if r = callAPI(h, "PUT", "/api/v1/users/"+bid+"/block", `{"blocked":true}`, ""); r.Code != 401 {
		t.Fatalf("unauthenticated block: %d", r.Code)
	}
	otherResponse := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, cid), a)
	var otherChat chatView
	_ = json.Unmarshal(otherResponse.Body.Bytes(), &otherChat)
	if r = callAPI(h, "POST", "/api/v1/chats/"+otherChat.ID+"/messages", body, a); r.Code != 201 {
		t.Fatalf("unrelated pair blocked: %d %s", r.Code, r.Body.String())
	}

	if v := card(a); !v.Blocked || v.CanMessage || v.CanCall || v.Presence != nil {
		t.Fatalf("own block %+v", v)
	}
	if v := card(b); v.Blocked || v.CanMessage || v.CanCall || v.Presence != nil {
		t.Fatalf("peer block %+v", v)
	}
	for _, token := range []string{a, b} {
		for _, action := range []struct{ method, path, body string }{{"POST", base + "/messages", body}, {"POST", base + "/calls", `{"kind":"audio"}`}} {
			r = callAPI(h, action.method, action.path, action.body, token)
			if r.Code != 403 {
				t.Fatalf("blocked action %s: %d %s", action.path, r.Code, r.Body.String())
			}
		}
		r = callAPI(h, "GET", base+"/messages", "", token)
		if r.Code != 200 || !containsText(r.Body.String(), message.ID) {
			t.Fatalf("history lost %d %s", r.Code, r.Body.String())
		}
		r = callAPI(h, "GET", base, "", token)
		if r.Code != 200 || containsText(r.Body.String(), `"presence"`) {
			t.Fatalf("presence leak %d %s", r.Code, r.Body.String())
		}
	}
	if r = callAPI(h, "PATCH", base+"/messages/"+message.ID, `{"text":"edit"}`, a); r.Code != 403 {
		t.Fatalf("blocked edit: %d %s", r.Code, r.Body.String())
	}
	for _, token := range []string{a, b} {
		if r = callAPI(h, "GET", "/api/v1/files/"+fileID, "", token); r.Code != 200 {
			t.Fatalf("blocked history attachment: %d %s", r.Code, r.Body.String())
		}
		req := httptest.NewRequest("POST", base+"/files", strings.NewReader("file"))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("X-File-Name", "file.txt")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("blocked upload: %d %s", w.Code, w.Body.String())
		}
	}
	key, err := contentcrypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	encrypted := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, Objects: objects.New(config.Config{}), ContentKey: key})
	if r = callAPI(encrypted, "PUT", "/api/v1/files/"+fileID+"/preview", "", a); r.Code != 403 {
		t.Fatalf("blocked preview upload: %d %s", r.Code, r.Body.String())
	}
	// A shared group remains usable.
	r = callAPI(h, "POST", "/api/v1/chats", `{"title":"Family"}`, a)
	var group chatView
	_ = json.Unmarshal(r.Body.Bytes(), &group)
	if _, err := db.Exec(t.Context(), `INSERT INTO chat_members(chat_id,user_id,role) VALUES($1,$2,'member')`, group.ID, bid); err != nil {
		t.Fatal(err)
	}
	if r = callAPI(h, "POST", "/api/v1/chats/"+group.ID+"/messages", body, b); r.Code != 201 {
		t.Fatalf("group blocked: %d %s", r.Code, r.Body.String())
	}
	if r = callAPI(h, "GET", "/api/v1/chats/"+group.ID+"/contact", "", a); r.Code != 404 {
		t.Fatalf("group card: %d", r.Code)
	}
	set(aid, b, true)
	set(bid, a, false)
	if v := card(a); v.Blocked || v.CanMessage {
		t.Fatalf("unblock removed peer edge: %+v", v)
	}
	set(aid, b, false)
	if v := card(a); !v.CanMessage || !v.CanCall || v.Presence == nil {
		t.Fatalf("unblock: %+v", v)
	}
	for _, target := range []string{aid, "bad", "00000000-0000-4000-8000-000000000099"} {
		r = callAPI(h, "PUT", "/api/v1/users/"+target+"/block", `{"blocked":true}`, a)
		if r.Code != 400 && r.Code != 404 {
			t.Fatalf("invalid block %s: %d", target, r.Code)
		}
	}
	if r = callAPI(h, "PUT", "/api/v1/users/"+bid+"/block", `{}`, a); r.Code != 400 {
		t.Fatalf("missing blocked: %d", r.Code)
	}
	if _, err := db.Exec(t.Context(), `UPDATE users SET disabled=true WHERE id=$1`, bid); err != nil {
		t.Fatal(err)
	}
	if v := card(a); v.CanMessage || v.CanCall {
		t.Fatalf("disabled contact: %+v", v)
	}
}

func TestBlockingTerminatesDirectCallsAndPendingVoIPJobs(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			db := testDatabase(t)
			aid, a := seedSession(t, db, "user")
			bid, b := seedSession(t, db, "user")
			h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, PushEnabled: true})
			var device string
			if err := db.QueryRow(t.Context(), `INSERT INTO push_devices(session_id,user_id,client_server_id,device_id,voip_token) SELECT id,user_id,gen_random_uuid(),gen_random_uuid(),'block-test' FROM sessions WHERE user_id=$1 LIMIT 1 RETURNING id::text`, bid).Scan(&device); err != nil {
				t.Fatal(err)
			}
			r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, bid), a)
			var chat chatView
			_ = json.Unmarshal(r.Body.Bytes(), &chat)
			r = callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/calls", `{"kind":"audio"}`, a)
			var call callView
			_ = json.Unmarshal(r.Body.Bytes(), &call)
			if r.Code != 201 {
				t.Fatalf("call: %d %s", r.Code, r.Body.String())
			}
			if accepted {
				r = callAPI(h, "POST", "/api/v1/calls/"+call.ID+"/accept", "", b)
				if r.Code != 200 {
					t.Fatalf("accept: %d", r.Code)
				}
			}
			r = callAPI(h, "PUT", "/api/v1/users/"+aid+"/block", `{"blocked":true}`, b)
			if r.Code != 204 {
				t.Fatalf("block call: %d %s", r.Code, r.Body.String())
			}
			var state string
			var pending int
			if err := db.QueryRow(t.Context(), `SELECT state,(SELECT count(*) FROM push_jobs WHERE payload->>'call_id'=$1) FROM calls WHERE id=$1::uuid`, call.ID).Scan(&state, &pending); err != nil {
				t.Fatal(err)
			}
			want := "cancelled"
			if accepted {
				want = "ended"
			}
			if state != want || pending != 0 {
				t.Fatalf("active call leaked state=%s jobs=%d", state, pending)
			}
			if r = callAPI(h, "POST", "/api/v1/calls/"+call.ID+"/accept", "", b); r.Code != 403 {
				t.Fatalf("blocked accept: %d %s", r.Code, r.Body.String())
			}
			if r = callAPI(h, "GET", "/api/v1/calls", "", a); r.Code != 200 || !containsText(r.Body.String(), `"can_redial":false`) {
				t.Fatalf("blocked redial: %d %s", r.Code, r.Body.String())
			}
		})
	}
}

func TestBlockAndCallAcceptanceUseChatFirstLockOrder(t *testing.T) {
	db := testDatabase(t)
	aid, a := seedSession(t, db, "user")
	bid, b := seedSession(t, db, "user")
	hub := events.NewHub(db)
	deps := Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, Events: hub}
	h := New(deps)
	srv := httptest.NewServer(h)
	defer srv.Close()
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, bid), a)
	var chat chatView
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	r = callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/calls", `{"kind":"audio"}`, a)
	var call callView
	_ = json.Unmarshal(r.Body.Bytes(), &call)
	// Hold the common chat lock to queue both operations together. They must
	// serialize without the former call-row/chat-row lock inversion.
	tx, err := db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM chats WHERE id=$1 FOR UPDATE`, chat.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 2)
	go func() { done <- callAPI(h, "POST", "/api/v1/calls/"+call.ID+"/accept", "", b).Code }()
	go func() { done <- callAPI(h, "PUT", "/api/v1/users/"+aid+"/block", `{"blocked":true}`, b).Code }()
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case status := <-done:
			if status != 200 && status != 204 && status != 403 {
				t.Fatalf("racing action failed: %d", status)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("block/accept deadlock")
		}
	}
	var state string
	if err = db.QueryRow(t.Context(), `SELECT state FROM calls WHERE id=$1`, call.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "cancelled" && state != "ended" {
		t.Fatalf("call survived block: %s", state)
	}
	// A blocked pair cannot relay SDP/ICE even if an old accepted row remains.
	if _, err = db.Exec(t.Context(), `UPDATE calls SET state='accepted',accepted_session_id=(SELECT id FROM sessions WHERE user_id=$2 LIMIT 1) WHERE id=$1`, call.ID, bid); err != nil {
		t.Fatal(err)
	}
	var session string
	if err = db.QueryRow(t.Context(), `SELECT id::text FROM sessions WHERE user_id=$1 LIMIT 1`, aid).Scan(&session); err != nil {
		t.Fatal(err)
	}
	rr := callAPI(h, "POST", "/api/v1/auth/websocket-ticket", "", b)
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &ticket)
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/ws?ticket="+ticket.Ticket, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var targetSession string
	if err = db.QueryRow(t.Context(), `SELECT accepted_session_id::text FROM calls WHERE id=$1`, call.ID).Scan(&targetSession); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !hub.HasSession(targetSession) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hub.HasSession(targetSession) {
		t.Fatal("target socket not attached")
	}
	if relaySignal(context.Background(), deps, auth.Principal{UserID: aid, SessionID: session}, "offer", call.ID, json.RawMessage(`{"sdp":"blocked"}`)) {
		t.Fatal("relayed blocked signal")
	}
	r = callAPI(h, "PUT", "/api/v1/users/"+aid+"/block", `{"blocked":false}`, b)
	if r.Code != 204 {
		t.Fatalf("unblock signal %d", r.Code)
	}
	if !relaySignal(context.Background(), deps, auth.Principal{UserID: aid, SessionID: session}, "offer", call.ID, json.RawMessage(`{"sdp":"allowed"}`)) {
		t.Fatal("unblocked signal not relayed")
	}
	readCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, payload, err := conn.Read(readCtx)
	if err != nil || !strings.Contains(string(payload), "allowed") {
		t.Fatalf("signal %s %v", payload, err)
	}
}
