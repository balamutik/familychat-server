package httpapi

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"familychat/server/internal/auth"
)

func TestMessageIdempotencyHistorySearchAndRead(t *testing.T) {
	pool := testDatabase(t)
	a, at := seedSession(t, pool, "user")
	b, bt := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, b), at)
	if r.Code != 201 {
		t.Fatalf("direct chat: %d", r.Code)
	}
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	url := "/api/v1/chats/" + chat.ID
	body := `{"client_message_id":"00000000-0000-4000-8000-000000000001","text":"Привет, семья!"}`
	r = callAPI(h, "POST", url+"/messages", body, at)
	if r.Code != 201 {
		t.Fatalf("send: %d %s", r.Code, r.Body.String())
	}
	var first struct {
		ID  string `json:"id"`
		Seq int64  `json:"seq"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &first)
	if first.ID == "" || first.Seq != 1 {
		t.Fatalf("first message: %+v", first)
	}
	r = callAPI(h, "POST", url+"/messages", body, at)
	var repeat struct {
		ID  string `json:"id"`
		Seq int64  `json:"seq"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &repeat)
	if r.Code != 200 || repeat.ID != first.ID || repeat.Seq != 1 {
		t.Fatalf("duplicate: %d %+v", r.Code, repeat)
	}
	conflict := `{"client_message_id":"00000000-0000-4000-8000-000000000001","text":"Подмена"}`
	if r := callAPI(h, "POST", url+"/messages", conflict, at); r.Code != 409 {
		t.Fatalf("conflicting duplicate: %d", r.Code)
	}
	if r := callAPI(h, "GET", url+"/messages?after=0", "", bt); r.Code != 200 || !containsText(r.Body.String(), "Привет, семья!") {
		t.Fatalf("history: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "GET", url+"/search?q=Привет", "", bt); r.Code != 200 || !containsText(r.Body.String(), "Привет, семья!") {
		t.Fatalf("search: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "POST", url+"/read", `{"seq":1}`, bt); r.Code != 200 || !containsText(r.Body.String(), `"read_seq":1`) {
		t.Fatalf("read: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "POST", url+"/read", `{"seq":0}`, bt); r.Code != 200 || !containsText(r.Body.String(), `"read_seq":1`) {
		t.Fatalf("read regression: %d %s", r.Code, r.Body.String())
	}
	_ = a
}

func TestReplyReferencesRemainInHistoryAndRejectForeignMessages(t *testing.T) {
	pool := testDatabase(t)
	a, at := seedSession(t, pool, "user")
	b, bt := seedSession(t, pool, "user")
	c, _ := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}})
	chat := func(peer string) string {
		r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, peer), at)
		if r.Code != 201 {
			t.Fatalf("create chat: %d %s", r.Code, r.Body.String())
		}
		var result struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.ID
	}
	chatID, otherChatID := chat(b), chat(c)
	url := "/api/v1/chats/" + chatID + "/messages"
	original := callAPI(h, "POST", url, `{"client_message_id":"00000000-0000-4000-8000-000000000101","text":"original"}`, at)
	if original.Code != 201 {
		t.Fatalf("original: %d %s", original.Code, original.Body.String())
	}
	var first messageView
	if err := json.Unmarshal(original.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	replyBody := fmt.Sprintf(`{"client_message_id":"00000000-0000-4000-8000-000000000102","text":"reply","reply_to_message_id":%q}`, first.ID)
	reply := callAPI(h, "POST", url, replyBody, bt)
	if reply.Code != 201 {
		t.Fatalf("reply: %d %s", reply.Code, reply.Body.String())
	}
	var sent messageView
	if err := json.Unmarshal(reply.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ReplyToMessageID == nil || *sent.ReplyToMessageID != first.ID {
		t.Fatalf("reply reference missing: %+v", sent)
	}
	for _, path := range []string{url, url + "/" + sent.ID, "/api/v1/chats/" + chatID + "/search?q=reply"} {
		r := callAPI(h, "GET", path, "", at)
		if r.Code != 200 || !containsText(r.Body.String(), `"reply_to_message_id":"`+first.ID+`"`) {
			t.Fatalf("reply reference missing from %s: %d %s", path, r.Code, r.Body.String())
		}
	}
	if r := callAPI(h, "POST", url, replyBody, bt); r.Code != 200 {
		t.Fatalf("idempotent retry: %d", r.Code)
	}
	changed := fmt.Sprintf(`{"client_message_id":"00000000-0000-4000-8000-000000000102","text":"reply","reply_to_message_id":null}`)
	if r := callAPI(h, "POST", url, changed, bt); r.Code != 409 {
		t.Fatalf("changed reply target: %d", r.Code)
	}
	foreign := callAPI(h, "POST", "/api/v1/chats/"+otherChatID+"/messages", `{"client_message_id":"00000000-0000-4000-8000-000000000103","text":"foreign"}`, at)
	var other messageView
	if err := json.Unmarshal(foreign.Body.Bytes(), &other); err != nil {
		t.Fatal(err)
	}
	bad := fmt.Sprintf(`{"client_message_id":"00000000-0000-4000-8000-000000000104","text":"bad","reply_to_message_id":%q}`, other.ID)
	if r := callAPI(h, "POST", url, bad, bt); r.Code != 404 {
		t.Fatalf("cross-chat reply: %d", r.Code)
	}
	_ = a
}

func TestForwardedFlagSurvivesHistorySearchAndRetry(t *testing.T) {
	pool := testDatabase(t)
	_, senderToken := seedSession(t, pool, "user")
	recipientID, recipientToken := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}})
	created := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, recipientID), senderToken)
	if created.Code != 201 {
		t.Fatalf("chat: %d %s", created.Code, created.Body.String())
	}
	var chat struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &chat); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/chats/" + chat.ID + "/messages"
	body := `{"client_message_id":"00000000-0000-4000-8000-000000000201","text":"forwarded text","forwarded":true}`
	sent := callAPI(h, "POST", path, body, senderToken)
	if sent.Code != 201 || !containsText(sent.Body.String(), `"forwarded":true`) {
		t.Fatalf("send: %d %s", sent.Code, sent.Body.String())
	}
	for _, request := range []struct{ method, path, token string }{
		{"POST", path, senderToken},
		{"GET", path, recipientToken},
		{"GET", "/api/v1/chats/" + chat.ID + "/search?q=forwarded", recipientToken},
	} {
		payload := ""
		if request.method == "POST" {
			payload = body
		}
		response := callAPI(h, request.method, request.path, payload, request.token)
		if response.Code != 200 || !containsText(response.Body.String(), `"forwarded":true`) {
			t.Fatalf("%s %s: %d %s", request.method, request.path, response.Code, response.Body.String())
		}
	}
	changed := `{"client_message_id":"00000000-0000-4000-8000-000000000201","text":"forwarded text","forwarded":false}`
	if response := callAPI(h, "POST", path, changed, senderToken); response.Code != 409 {
		t.Fatalf("changed forwarded flag: %d %s", response.Code, response.Body.String())
	}
}

func containsText(s, substring string) bool {
	return len(s) >= len(substring) && (stringContains(s, substring))
}

func stringContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
