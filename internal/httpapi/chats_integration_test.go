package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"familychat/server/internal/auth"
)

func TestConcurrentDirectChatCreation(t *testing.T) {
	pool := testDatabase(t)
	a, token := seedSession(t, pool, "user")
	b, _ := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}})
	var wg sync.WaitGroup
	ids := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, b), token)
			if r.Code != 200 && r.Code != 201 {
				ids <- fmt.Sprintf("status:%d", r.Code)
				return
			}
			var v struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(r.Body.Bytes(), &v)
			ids <- v.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first || id == "" {
			t.Fatalf("inconsistent direct chats: first=%q got=%q", first, id)
		}
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM chats WHERE kind='direct' AND ((direct_user_low=$1 AND direct_user_high=$2) OR (direct_user_low=$2 AND direct_user_high=$1))`, a, b).Scan(&count); err != nil || count != 1 {
		t.Fatalf("chat count=%d err=%v", count, err)
	}
	var peerLogin string
	if err := pool.QueryRow(t.Context(), `SELECT login FROM users WHERE id=$1`, b).Scan(&peerLogin); err != nil {
		t.Fatal(err)
	}
	if r := callAPI(h, "GET", "/api/v1/users?q="+peerLogin, "", token); r.Code != 200 || !containsText(r.Body.String(), b) {
		t.Fatalf("user discovery: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "GET", "/api/v1/chats/"+first, "", token); r.Code != 200 || !containsText(r.Body.String(), peerLogin) {
		t.Fatalf("direct title: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "GET", "/api/v1/users?q="+peerLogin, "", ""); r.Code != 401 {
		t.Fatalf("anonymous discovery: %d", r.Code)
	}
}

func TestChatUnreadCountTracksReceivedMessagesAndReadPosition(t *testing.T) {
	pool := testDatabase(t)
	_, senderToken := seedSession(t, pool, "user")
	recipientID, recipientToken := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, recipientID), senderToken)
	if r.Code != 201 {
		t.Fatalf("create direct chat: %d %s", r.Code, r.Body.String())
	}
	var chat struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &chat); err != nil {
		t.Fatal(err)
	}
	url := "/api/v1/chats/" + chat.ID

	unread := func(token string) int64 {
		t.Helper()
		r := callAPI(h, "GET", "/api/v1/chats", "", token)
		if r.Code != 200 {
			t.Fatalf("list chats: %d %s", r.Code, r.Body.String())
		}
		var result struct {
			Chats []struct {
				ID          string `json:"id"`
				UnreadCount int64  `json:"unread_count"`
			} `json:"chats"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		for _, item := range result.Chats {
			if item.ID == chat.ID {
				return item.UnreadCount
			}
		}
		t.Fatalf("chat %s missing from list", chat.ID)
		return -1
	}
	send := func(token, id string) {
		t.Helper()
		r := callAPI(h, "POST", url+"/messages", fmt.Sprintf(`{"client_message_id":%q,"text":"hello"}`, id), token)
		if r.Code != 201 {
			t.Fatalf("send message: %d %s", r.Code, r.Body.String())
		}
	}

	if got := unread(recipientToken); got != 0 {
		t.Fatalf("new chat unread=%d, want 0", got)
	}
	send(senderToken, "00000000-0000-4000-8000-000000000011")
	send(senderToken, "00000000-0000-4000-8000-000000000012")
	if got := unread(recipientToken); got != 2 {
		t.Fatalf("recipient unread=%d, want 2", got)
	}
	if got := unread(senderToken); got != 0 {
		t.Fatalf("sender unread=%d, want 0", got)
	}
	if r := callAPI(h, "POST", url+"/read", `{"seq":1}`, recipientToken); r.Code != 200 {
		t.Fatalf("mark first read: %d %s", r.Code, r.Body.String())
	}
	if got := unread(recipientToken); got != 1 {
		t.Fatalf("partly read unread=%d, want 1", got)
	}
	send(recipientToken, "00000000-0000-4000-8000-000000000013")
	if got := unread(recipientToken); got != 1 {
		t.Fatalf("own message changed unread=%d, want 1", got)
	}
	if got := unread(senderToken); got != 1 {
		t.Fatalf("other sender unread=%d, want 1", got)
	}
	if r := callAPI(h, "POST", url+"/read", `{"seq":3}`, recipientToken); r.Code != 200 {
		t.Fatalf("mark all read: %d %s", r.Code, r.Body.String())
	}
	if got := unread(recipientToken); got != 0 {
		t.Fatalf("read chat unread=%d, want 0", got)
	}
}

func TestChatListIncludesLatestMessagePreviewAndTime(t *testing.T) {
	pool := testDatabase(t)
	senderID, senderToken := seedSession(t, pool, "user")
	recipientID, recipientToken := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, recipientID), senderToken)
	if r.Code != 201 {
		t.Fatalf("create chat: %d %s", r.Code, r.Body.String())
	}
	var chat struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &chat); err != nil {
		t.Fatal(err)
	}
	list := func() map[string]any {
		t.Helper()
		r := callAPI(h, "GET", "/api/v1/chats", "", recipientToken)
		if r.Code != 200 {
			t.Fatalf("list chats: %d %s", r.Code, r.Body.String())
		}
		var result struct {
			Chats []map[string]any `json:"chats"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		for _, item := range result.Chats {
			if item["id"] == chat.ID {
				return item
			}
		}
		t.Fatalf("chat %s missing", chat.ID)
		return nil
	}
	if got := list()["last_message_preview"]; got != nil {
		t.Fatalf("empty chat preview=%v", got)
	}
	thirdID, _ := seedSession(t, pool, "user")
	r = callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, thirdID), senderToken)
	if r.Code != 201 {
		t.Fatalf("create second chat: %d %s", r.Code, r.Body.String())
	}
	r = callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/messages", `{"client_message_id":"00000000-0000-4000-8000-000000000031","text":"Первое"}`, senderToken)
	if r.Code != 201 {
		t.Fatalf("first message: %d %s", r.Code, r.Body.String())
	}
	r = callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/messages", `{"client_message_id":"00000000-0000-4000-8000-000000000032","text":"Новое сообщение"}`, senderToken)
	if r.Code != 201 {
		t.Fatalf("second message: %d %s", r.Code, r.Body.String())
	}
	item := list()
	if item["last_message_preview"] != "Новое сообщение" {
		t.Fatalf("preview=%v", item["last_message_preview"])
	}
	if item["last_message_at"] == nil || item["last_message_at"] == "" {
		t.Fatalf("missing message time: %v", item)
	}
	var senderLogin string
	if err := pool.QueryRow(t.Context(), `SELECT login FROM users WHERE id=$1`, senderID).Scan(&senderLogin); err != nil {
		t.Fatal(err)
	}
	if item["last_message_sender"] != senderLogin {
		t.Fatalf("sender=%v, want %s", item["last_message_sender"], senderLogin)
	}
	r = callAPI(h, "GET", "/api/v1/chats", "", senderToken)
	if r.Code != 200 {
		t.Fatalf("list sender chats: %d %s", r.Code, r.Body.String())
	}
	var ordered struct {
		Chats []struct {
			ID string `json:"id"`
		} `json:"chats"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &ordered); err != nil {
		t.Fatal(err)
	}
	if len(ordered.Chats) < 2 || ordered.Chats[0].ID != chat.ID {
		t.Fatalf("updated chat should be first: %+v", ordered.Chats)
	}
	var fileID, messageID string
	if err := pool.QueryRow(t.Context(), `INSERT INTO attachments(chat_id,uploader_id,object_key,filename,content_type,size_bytes,preview_state)
		VALUES($1,$2,$3,'семейное фото.jpg','image/jpeg',5,'pending') RETURNING id::text`, chat.ID, senderID, "tests/preview-"+chat.ID).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `INSERT INTO messages(chat_id,seq,sender_id,client_message_id,body)
		VALUES($1,3,$2,gen_random_uuid(),'') RETURNING id::text`, chat.ID, senderID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO message_attachments(message_id,attachment_id) VALUES($1,$2)`, messageID, fileID); err != nil {
		t.Fatal(err)
	}
	if got := list()["last_message_preview"]; got != "семейное фото.jpg" {
		t.Fatalf("attachment preview=%v", got)
	}
}

func TestGroupRolesAndRemovedMember(t *testing.T) {
	pool := testDatabase(t)
	owner, ownerToken := seedSession(t, pool, "user")
	member, memberToken := seedSession(t, pool, "user")
	outsider, outToken := seedSession(t, pool, "admin")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}})
	r := callAPI(h, "POST", "/api/v1/chats", `{"title":"Семья"}`, ownerToken)
	if r.Code != 201 {
		t.Fatalf("create group: %d %s", r.Code, r.Body.String())
	}
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	url := "/api/v1/chats/" + chat.ID
	if r := callAPI(h, "GET", url, "", outToken); r.Code != 404 {
		t.Fatalf("outsider admin read=%d", r.Code)
	}
	if r := callAPI(h, "POST", url+"/members", fmt.Sprintf(`{"user_id":%q}`, member), memberToken); r.Code != 404 {
		t.Fatalf("non-member self add=%d", r.Code)
	}
	if r := callAPI(h, "POST", url+"/members", fmt.Sprintf(`{"user_id":%q}`, member), ownerToken); r.Code != 201 {
		t.Fatalf("owner add=%d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "GET", url, "", memberToken); r.Code != 200 {
		t.Fatalf("member read=%d", r.Code)
	}
	if r := callAPI(h, "GET", url+"/members", "", ownerToken); r.Code != 200 || !containsText(r.Body.String(), member) {
		t.Fatalf("group member list: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "GET", url+"/members", "", outToken); r.Code != 404 {
		t.Fatalf("outsider members: %d", r.Code)
	}
	if r := callAPI(h, "DELETE", url+"/members/"+member, "", ownerToken); r.Code != 204 {
		t.Fatalf("remove=%d", r.Code)
	}
	if r := callAPI(h, "GET", url, "", memberToken); r.Code != 404 {
		t.Fatalf("removed member read=%d", r.Code)
	}
	if r := callAPI(h, "GET", url+"/members", "", memberToken); r.Code != 404 {
		t.Fatalf("removed member list: %d", r.Code)
	}
	if r := callAPI(h, "POST", url+"/leave", "", ownerToken); r.Code != http.StatusConflict {
		t.Fatalf("owner left without transfer: %d", r.Code)
	}
	_ = owner
	_ = outsider
}
