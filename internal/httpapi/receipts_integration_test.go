package httpapi

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"familychat/server/internal/auth"
)

func TestMessageDeliveryAndReadReceipts(t *testing.T) {
	pool := testDatabase(t)
	sender, senderToken := seedSession(t, pool, "user")
	first, firstToken := seedSession(t, pool, "user")
	second, secondToken := seedSession(t, pool, "user")
	_, outsiderToken := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}})
	r := callAPI(h, "POST", "/api/v1/chats", `{"title":"Receipts"}`, senderToken)
	if r.Code != 201 {
		t.Fatalf("create chat: %d %s", r.Code, r.Body.String())
	}
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	path := "/api/v1/chats/" + chat.ID
	for _, id := range []string{first, second} {
		if r := callAPI(h, "POST", path+"/members", fmt.Sprintf(`{"user_id":%q}`, id), senderToken); r.Code != 201 {
			t.Fatalf("add member: %d", r.Code)
		}
	}
	r = callAPI(h, "POST", path+"/messages", `{"client_message_id":"00000000-0000-4000-8000-000000000031","text":"Hello"}`, senderToken)
	if r.Code != 201 {
		t.Fatalf("send: %d %s", r.Code, r.Body.String())
	}
	check := func(want string) {
		t.Helper()
		response := callAPI(h, "GET", path+"/messages", "", senderToken)
		if response.Code != 200 || !containsText(response.Body.String(), want) {
			t.Fatalf("history lacks %s: %d %s", want, response.Code, response.Body.String())
		}
	}
	check(`"status":"sent"`)
	if r := callAPI(h, "POST", path+"/delivered", `{"seq":1}`, outsiderToken); r.Code != 404 {
		t.Fatalf("outsider receipt: %d", r.Code)
	}
	if r := callAPI(h, "POST", path+"/delivered", `{"seq":-1}`, firstToken); r.Code != 400 {
		t.Fatalf("negative seq: %d", r.Code)
	}
	if r := callAPI(h, "POST", path+"/delivered", `{"seq":999}`, firstToken); r.Code != 200 || !containsText(r.Body.String(), `"delivered_seq":1`) {
		t.Fatalf("first delivery: %d %s", r.Code, r.Body.String())
	}
	check(`"delivered_count":1`)
	if r := callAPI(h, "POST", path+"/read", `{"seq":1}`, secondToken); r.Code != 200 || !containsText(r.Body.String(), `"delivered_seq":1`) || !containsText(r.Body.String(), `"read_seq":1`) {
		t.Fatalf("second read: %d %s", r.Code, r.Body.String())
	}
	check(`"status":"delivered"`)
	check(`"read_count":1`)
	if r := callAPI(h, "POST", path+"/read", `{"seq":1}`, firstToken); r.Code != 200 {
		t.Fatalf("first read: %d", r.Code)
	}
	check(`"status":"read"`)
	check(fmt.Sprintf(`"user_id":"%s"`, first))
	check(fmt.Sprintf(`"user_id":"%s"`, second))
	late, _ := seedSession(t, pool, "user")
	if r := callAPI(h, "POST", path+"/members", fmt.Sprintf(`{"user_id":%q}`, late), senderToken); r.Code != 201 {
		t.Fatalf("add late member: %d %s", r.Code, r.Body.String())
	}
	check(`"status":"read"`)
	check(`"recipient_count":2`)
	if r := callAPI(h, "POST", path+"/delivered", `{"seq":0}`, firstToken); r.Code != 200 || !containsText(r.Body.String(), `"delivered_seq":1`) {
		t.Fatalf("receipt regressed: %d %s", r.Code, r.Body.String())
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE chat_id=$1 AND kind='receipt'`, chat.ID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("receipt events=%d err=%v", count, err)
	}
	_ = sender
}
