package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/config"
	"familychat/server/internal/objects"
)

func TestFileAuthorizationAndRange(t *testing.T) {
	endpoint := testS3Endpoint(t)
	pool := testDatabase(t)
	sender, st := seedSession(t, pool, "user")
	recipient, rt := seedSession(t, pool, "user")
	_, outsiderToken := seedSession(t, pool, "admin")
	store := objects.New(config.Config{S3Endpoint: endpoint, S3Region: "us-east-1", S3Bucket: "familychat-test", S3AccessKey: "familychat_test", S3SecretKey: "familychat_test_secret", S3UsePathStyle: true})
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, Objects: store})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, recipient), st)
	if r.Code != 201 {
		t.Fatalf("create direct: %d", r.Code)
	}
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	upload := httptest.NewRequest("POST", "/api/v1/chats/"+chat.ID+"/files", strings.NewReader("hello world"))
	upload.Header.Set("Authorization", "Bearer "+st)
	upload.Header.Set("Content-Type", "text/plain")
	upload.Header.Set("X-File-Name", "hello.txt")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, upload)
	if w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var file struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &file)
	path := "/api/v1/files/" + file.ID + "/content"
	defer func() {
		var key string
		if pool.QueryRow(t.Context(), `SELECT object_key FROM attachments WHERE id=$1`, file.ID).Scan(&key) == nil {
			_ = store.Delete(t.Context(), key)
		}
	}()
	if r := callAPI(h, "GET", path, "", rt); r.Code != 404 {
		t.Fatalf("recipient before message: %d", r.Code)
	}
	message := fmt.Sprintf(`{"client_message_id":"00000000-0000-4000-8000-000000000002","text":"Файл","attachment_ids":[%q]}`, file.ID)
	if r := callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/messages", message, st); r.Code != 201 {
		t.Fatalf("send attachment: %d %s", r.Code, r.Body.String())
	}
	for _, tc := range []struct {
		name, method, token string
		status              int
	}{
		{"recipient", "GET", rt, 200}, {"recipient HEAD", "HEAD", rt, 200}, {"sender", "GET", st, 200},
		{"no token", "GET", "", 401}, {"outsider admin", "GET", outsiderToken, 404},
	} {
		rr := callAPI(h, tc.method, path, "", tc.token)
		if rr.Code != tc.status {
			t.Fatalf("%s status=%d body=%s", tc.name, rr.Code, rr.Body.String())
		}
		if tc.method == "HEAD" && rr.Body.Len() != 0 {
			t.Fatal("HEAD returned a body")
		}
	}
	rangeReq := httptest.NewRequest("GET", path, nil)
	rangeReq.Header.Set("Authorization", "Bearer "+rt)
	rangeReq.Header.Set("Range", "bytes=6-10")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, rangeReq)
	if w.Code != 206 || w.Body.String() != "world" || w.Header().Get("Content-Range") != "bytes 6-10/11" {
		t.Fatalf("range: %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Range"))
	}
	queryToken := httptest.NewRequest("GET", path+"?token="+rt, nil)
	queryToken.AddCookie(&http.Cookie{Name: "session", Value: rt})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, queryToken)
	if w.Code != 401 {
		t.Fatalf("query/cookie token accepted: %d", w.Code)
	}
	_ = sender
}

func TestGroupFileAccessStopsAfterRemoval(t *testing.T) {
	endpoint := testS3Endpoint(t)
	pool := testDatabase(t)
	_, ownerToken := seedSession(t, pool, "user")
	memberID, memberToken := seedSession(t, pool, "user")
	_, strangerToken := seedSession(t, pool, "admin")
	store := objects.New(config.Config{S3Endpoint: endpoint, S3Region: "us-east-1", S3Bucket: "familychat-test", S3AccessKey: "familychat_test", S3SecretKey: "familychat_test_secret", S3UsePathStyle: true})
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, Objects: store})
	r := callAPI(h, "POST", "/api/v1/chats", `{"title":"Семья"}`, ownerToken)
	if r.Code != 201 {
		t.Fatalf("group: %d", r.Code)
	}
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	chatPath := "/api/v1/chats/" + chat.ID
	if r := callAPI(h, "POST", chatPath+"/members", fmt.Sprintf(`{"user_id":%q}`, memberID), ownerToken); r.Code != 201 {
		t.Fatalf("add member: %d", r.Code)
	}
	upload := httptest.NewRequest("POST", chatPath+"/files", strings.NewReader("private bytes"))
	upload.Header.Set("Authorization", "Bearer "+ownerToken)
	upload.Header.Set("X-File-Name", "private.txt")
	upload.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, upload)
	if w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var file struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &file)
	defer func() {
		var key string
		if pool.QueryRow(t.Context(), `SELECT object_key FROM attachments WHERE id=$1`, file.ID).Scan(&key) == nil {
			_ = store.Delete(t.Context(), key)
		}
	}()
	path := "/api/v1/files/" + file.ID + "/content"
	if r := callAPI(h, "GET", path, "", memberToken); r.Code != 404 {
		t.Fatalf("unattached group file exposed: %d", r.Code)
	}
	body := fmt.Sprintf(`{"client_message_id":"00000000-0000-4000-8000-000000000003","text":"Для семьи","attachment_ids":[%q]}`, file.ID)
	if r := callAPI(h, "POST", chatPath+"/messages", body, ownerToken); r.Code != 201 {
		t.Fatalf("message: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "GET", path, "", memberToken); r.Code != 200 {
		t.Fatalf("member file: %d", r.Code)
	}
	if r := callAPI(h, "GET", path, "", strangerToken); r.Code != 404 {
		t.Fatalf("admin stranger file: %d", r.Code)
	}
	if r := callAPI(h, "DELETE", chatPath+"/members/"+memberID, "", ownerToken); r.Code != 204 {
		t.Fatalf("remove member: %d", r.Code)
	}
	for _, method := range []string{"GET", "HEAD"} {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+memberToken)
		req.Header.Set("Range", "bytes=0-2")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 404 || w.Header().Get("Content-Range") != "" {
			t.Fatalf("removed member %s: %d %q", method, w.Code, w.Header().Get("Content-Range"))
		}
	}
}

func testS3Endpoint(t *testing.T) string {
	t.Helper()
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set TEST_S3_ENDPOINT")
	}
	return endpoint
}
