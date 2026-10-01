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
