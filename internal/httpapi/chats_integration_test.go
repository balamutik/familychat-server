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
	if r := callAPI(h, "DELETE", url+"/members/"+member, "", ownerToken); r.Code != 204 {
		t.Fatalf("remove=%d", r.Code)
	}
	if r := callAPI(h, "GET", url, "", memberToken); r.Code != 404 {
		t.Fatalf("removed member read=%d", r.Code)
	}
	if r := callAPI(h, "POST", url+"/leave", "", ownerToken); r.Code != http.StatusConflict {
		t.Fatalf("owner left without transfer: %d", r.Code)
	}
	_ = owner
	_ = outsider
}
