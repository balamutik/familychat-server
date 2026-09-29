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
