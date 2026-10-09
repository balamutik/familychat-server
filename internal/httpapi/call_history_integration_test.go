package httpapi

import (
	"encoding/json"
	"familychat/server/internal/auth"
	"fmt"
	"testing"
	"time"
)

func TestCallHistoryVisibilityOutcomesAndRedial(t *testing.T) {
	pool := testDatabase(t)
	_, caller := seedSession(t, pool, "user")
	calleeID, callee := seedSession(t, pool, "user")
	_, outsider := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, calleeID), caller)
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	start := func() string {
		t.Helper()
		r := callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/calls", `{"kind":"video","media_tracking":true}`, caller)
		if r.Code != 201 {
			t.Fatalf("start: %d %s", r.Code, r.Body.String())
		}
		var v struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(r.Body.Bytes(), &v)
		return v.ID
	}
	callID := start()
	path := "/api/v1/calls/" + callID
	if r := callAPI(h, "POST", path+"/connected", "", caller); r.Code != 409 {
		t.Fatalf("ringing connected=%d", r.Code)
	}
	if r := callAPI(h, "POST", path+"/accept", "", callee); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if r := callAPI(h, "POST", path+"/connected", "", outsider); r.Code != 404 {
		t.Fatal(r.Code)
	}
	for range 2 {
		if r := callAPI(h, "POST", path+"/connected", "", caller); r.Code != 204 {
			t.Fatalf("connected=%d %s", r.Code, r.Body.String())
		}
	}
	if r := callAPI(h, "POST", path+"/end", "", caller); r.Code != 200 {
		t.Fatal(r.Code)
	}
	// A second accepted call with no media connection must not look successful.
	failedID := start()
	callAPI(h, "POST", "/api/v1/calls/"+failedID+"/accept", "", callee)
	callAPI(h, "POST", "/api/v1/calls/"+failedID+"/end", "", caller)
	cancelledID := start()
	callAPI(h, "POST", "/api/v1/calls/"+cancelledID+"/cancel", "", caller)
	type item struct {
		ID, Outcome, Direction string
		ChatID                 string `json:"chat_id"`
		Kind                   string
		PeerName               string `json:"peer_name"`
		CanRedial              bool   `json:"can_redial"`
	}
	list := func(token, query string) []item {
		t.Helper()
		r := callAPI(h, "GET", "/api/v1/calls"+query, "", token)
		if r.Code != 200 {
			t.Fatalf("list=%d %s", r.Code, r.Body.String())
		}
		var page struct {
			Items []item `json:"items"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page.Items
	}
	rows := list(caller, "")
	if len(rows) != 3 || rows[0].ID != cancelledID || rows[0].Outcome != "cancelled" || rows[1].Outcome != "failed" || rows[2].Outcome != "completed" {
		t.Fatalf("history=%+v", rows)
	}
	if rows[2].Direction != "outgoing" || rows[2].PeerName == "" || !rows[2].CanRedial || rows[2].ChatID != chat.ID || rows[2].Kind != "video" {
		t.Fatalf("redial metadata=%+v", rows[2])
	}
	if incoming := list(callee, ""); len(incoming) != 3 || incoming[0].Direction != "incoming" {
		t.Fatalf("incoming=%+v", incoming)
	}
	if got := list(outsider, ""); len(got) != 0 {
		t.Fatalf("leaked=%+v", got)
	}
	if older := list(caller, "?before="+failedID); len(older) != 1 || older[0].ID != callID {
		t.Fatalf("cursor=%+v", older)
	}
	if r := callAPI(h, "GET", "/api/v1/calls?before=invalid", "", caller); r.Code != 400 {
		t.Fatal(r.Code)
	}
	if r := callAPI(h, "GET", "/api/v1/calls", "", ""); r.Code != 401 {
		t.Fatal(r.Code)
	}
}

func TestCallHistoryPaginationLegacyAndUnavailablePeers(t *testing.T) {
	pool := testDatabase(t)
	callerID, caller := seedSession(t, pool, "user")
	calleeID, _ := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, calleeID), caller)
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	_, err := pool.Exec(t.Context(), `INSERT INTO calls(chat_id,caller_id,callee_id,kind,state,created_at,accepted_at,ended_at)
 SELECT $1,$2,$3,'audio','ended','2026-10-01'::timestamptz,'2026-10-01'::timestamptz,'2026-10-01'::timestamptz+interval '1 minute' FROM generate_series(1,52)`, chat.ID, callerID, calleeID)
	if err != nil {
		t.Fatal(err)
	}
	type page struct {
		Items []struct {
			ID, Outcome string
			CanRedial   bool `json:"can_redial"`
		} `json:"items"`
		Next *string `json:"next_cursor"`
	}
	var first, second page
	r = callAPI(h, "GET", "/api/v1/calls", "", caller)
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	_ = json.Unmarshal(r.Body.Bytes(), &first)
	if len(first.Items) != 50 || first.Next == nil {
		t.Fatalf("first=%+v", first)
	}
	r = callAPI(h, "GET", "/api/v1/calls?before="+*first.Next, "", caller)
	_ = json.Unmarshal(r.Body.Bytes(), &second)
	if len(second.Items) != 2 || second.Next != nil {
		t.Fatalf("second=%+v", second)
	}
	seen := map[string]bool{}
	for _, item := range append(first.Items, second.Items...) {
		if seen[item.ID] || item.Outcome != "unknown" {
			t.Fatalf("duplicate or legacy misclassified: %+v", item)
		}
		seen[item.ID] = true
	}
	if _, err := pool.Exec(t.Context(), `UPDATE users SET disabled=true WHERE id=$1`, calleeID); err != nil {
		t.Fatal(err)
	}
	r = callAPI(h, "GET", "/api/v1/calls", "", caller)
	_ = json.Unmarshal(r.Body.Bytes(), &first)
	if first.Items[0].CanRedial {
		t.Fatal("disabled peer can be called")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE chats SET deleted_at=now() WHERE id=$1`, chat.ID); err != nil {
		t.Fatal(err)
	}
	r = callAPI(h, "GET", "/api/v1/calls", "", caller)
	_ = json.Unmarshal(r.Body.Bytes(), &first)
	if len(first.Items) != 0 {
		t.Fatal("deleted chat history leaked")
	}
}
