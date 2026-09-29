package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"familychat/server/internal/auth"
)

func TestCallLifecycleBusyAndParticipants(t *testing.T) {
	pool := testDatabase(t)
	_, callerToken := seedSession(t, pool, "user")
	calleeID, calleeToken := seedSession(t, pool, "user")
	_, otherToken := seedSession(t, pool, "admin")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, TurnURL: "turn:127.0.0.1:3478", TurnSecret: "test-secret"})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, calleeID), callerToken)
	if r.Code != 201 {
		t.Fatalf("chat: %d", r.Code)
	}
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	startPath := "/api/v1/chats/" + chat.ID + "/calls"
	group:=callAPI(h,"POST","/api/v1/chats",`{"title":"Family"}`,callerToken)
	if group.Code!=201 { t.Fatal(group.Code) }; var groupView struct{ID string `json:"id"`}; _=json.Unmarshal(group.Body.Bytes(),&groupView)
	if r:=callAPI(h,"POST","/api/v1/chats/"+groupView.ID+"/calls",`{"kind":"audio"}`,callerToken); r.Code!=404 { t.Fatalf("group call status=%d",r.Code) }
	r = callAPI(h, "POST", startPath, `{"kind":"video"}`, callerToken)
	if r.Code != 201 {
		t.Fatalf("start: %d %s", r.Code, r.Body.String())
	}
	var call struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &call)
	if call.ID == "" || call.State != "ringing" {
		t.Fatalf("call=%+v", call)
	}
	path := "/api/v1/calls/" + call.ID
	if r := callAPI(h, "GET", path, "", otherToken); r.Code != 404 {
		t.Fatalf("outsider call: %d", r.Code)
	}
	if r := callAPI(h, "POST", startPath, `{"kind":"audio"}`, callerToken); r.Code != 409 {
		t.Fatalf("caller not busy: %d", r.Code)
	}
	otherDeviceToken, otherDeviceHash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(user_id,token_hash,expires_at) VALUES($1,$2,now()+interval '1 hour')`, calleeID, otherDeviceHash); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	type outcome struct {
		status int
		token  string
	}
	results := make(chan outcome, 2)
	for _, token := range []string{calleeToken, otherDeviceToken} {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			results <- outcome{callAPI(h, "POST", path+"/accept", "", token).Code, token}
		}(token)
	}
	wg.Wait()
	close(results)
	accepted, conflicts := 0, 0
	winner, loser := "", ""
	for result := range results {
		if result.status == 200 {
			accepted++
			winner = result.token
		} else if result.status == 409 {
			conflicts++
			loser = result.token
		} else {
			t.Errorf("accept status=%d", result.status)
		}
	}
	if accepted != 1 || conflicts != 1 {
		t.Fatalf("accept winners=%d conflicts=%d", accepted, conflicts)
	}
	if r := callAPI(h, "POST", path+"/accept", "", winner); r.Code != 200 {
		t.Fatalf("same-device retry: %d", r.Code)
	}
	if r := callAPI(h, "GET", path+"/ice", "", loser); r.Code != 404 {
		t.Fatalf("other device TURN credentials: %d", r.Code)
	}
	if r := callAPI(h, "GET", path+"/ice", "", winner); r.Code != 200 || !containsText(r.Body.String(), "credential") {
		t.Fatalf("TURN credentials: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "POST", path+"/end", "", callerToken); r.Code != 200 {
		t.Fatalf("end: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "GET", path+"/ice", "", winner); r.Code != 409 {
		t.Fatalf("TURN credentials after end: %d", r.Code)
	}
}

func TestCallTimeoutAndDisconnectSweep(t *testing.T) {
	pool := testDatabase(t)
	_, caller := seedSession(t, pool, "user")
	calleeID, callee := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, calleeID), caller)
	if r.Code != 201 {
		t.Fatal(r.Code)
	}
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	start := func() string {
		t.Helper()
		r := callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/calls", `{"kind":"audio"}`, caller)
		if r.Code != 201 {
			t.Fatalf("start %d %s", r.Code, r.Body.String())
		}
		var c struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(r.Body.Bytes(), &c)
		return c.ID
	}
	first := start()
	if _, err := pool.Exec(t.Context(), `UPDATE calls SET created_at=now()-interval '50 seconds' WHERE id=$1`, first); err != nil {
		t.Fatal(err)
	}
	if err := SweepCalls(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	r = callAPI(h, "GET", "/api/v1/calls/"+first, "", callee)
	if r.Code != 200 || !containsText(r.Body.String(), `"state":"missed"`) {
		t.Fatalf("missed %d %s", r.Code, r.Body.String())
	}
	second := start()
	if r := callAPI(h, "POST", "/api/v1/calls/"+second+"/accept", "", callee); r.Code != 200 {
		t.Fatalf("accept %d", r.Code)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE calls SET callee_disconnected_at=now()-interval '31 seconds' WHERE id=$1`, second); err != nil {
		t.Fatal(err)
	}
	if err := SweepCalls(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	r = callAPI(h, "GET", "/api/v1/calls/"+second, "", caller)
	if r.Code != 200 || !containsText(r.Body.String(), `"state":"ended"`) {
		t.Fatalf("disconnected %d %s", r.Code, r.Body.String())
	}
}
