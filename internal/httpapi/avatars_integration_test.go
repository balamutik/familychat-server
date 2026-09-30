package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/config"
	"familychat/server/internal/objects"
)

func TestAvatarUploadPublicReadReplaceAndDelete(t *testing.T) {
	endpoint := testS3Endpoint(t)
	pool := testDatabase(t)
	ownerID, ownerToken := seedSession(t, pool, "user")
	peerID, peerToken := seedSession(t, pool, "user")
	store := objects.New(config.Config{S3Endpoint: endpoint, S3Region: "us-east-1", S3Bucket: "familychat-test", S3AccessKey: "familychat_test", S3SecretKey: "familychat_test_secret", S3UsePathStyle: true})
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, Objects: store})
	path := "/api/v1/users/" + ownerID + "/avatar"
	put := func(token, contentType string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("PUT", "/api/v1/users/me/avatar", bytes.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if r := callAPI(h, "GET", path, "", ""); r.Code != 404 {
		t.Fatalf("missing avatar: %d", r.Code)
	}
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(0, 0, color.RGBA{R: 240, A: 255})
	var pngBytes, jpegBytes bytes.Buffer
	if err := png.Encode(&pngBytes, img); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegBytes, img, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, token, contentType string
		body                     []byte
		status                   int
	}{
		{"anonymous upload", "", "image/png", pngBytes.Bytes(), 401},
		{"svg", ownerToken, "image/svg+xml", []byte(`<svg/>`), 415},
		{"gif", ownerToken, "image/gif", []byte("GIF89a"), 415},
		{"spoofed image", ownerToken, "image/png", []byte("not an image"), 400},
		{"wrong MIME", ownerToken, "image/jpeg", pngBytes.Bytes(), 400},
	} {
		if r := put(tc.token, tc.contentType, tc.body); r.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.name, r.Code, r.Body.String())
		}
	}
	large := httptest.NewRequest("PUT", "/api/v1/users/me/avatar", strings.NewReader(strings.Repeat("x", 5<<20+1)))
	large.Header.Set("Content-Type", "image/png")
	large.Header.Set("Authorization", "Bearer "+ownerToken)
	largeResponse := httptest.NewRecorder()
	h.ServeHTTP(largeResponse, large)
	if largeResponse.Code != 413 {
		t.Fatalf("oversized avatar: %d %s", largeResponse.Code, largeResponse.Body.String())
	}
	first := put(ownerToken, "image/png", pngBytes.Bytes())
	if first.Code != 200 {
		t.Fatalf("upload PNG: %d %s", first.Code, first.Body.String())
	}
	var uploaded struct {
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &uploaded); err != nil || uploaded.AvatarURL != path {
		t.Fatalf("avatar URL: %+v %v", uploaded, err)
	}
	var firstKey string
	if err := pool.QueryRow(t.Context(), `SELECT avatar_key FROM users WHERE id=$1`, ownerID).Scan(&firstKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), firstKey) })
	for _, method := range []string{"GET", "HEAD"} {
		r := callAPI(h, method, path, "", "")
		if r.Code != 200 || r.Header().Get("Content-Type") != "image/png" || r.Header().Get("Location") != "" {
			t.Fatalf("public %s: %d headers=%v", method, r.Code, r.Header())
		}
		if method == "GET" && !bytes.Equal(r.Body.Bytes(), pngBytes.Bytes()) {
			t.Fatal("public PNG bytes differ")
		}
		if method == "HEAD" && r.Body.Len() != 0 {
			t.Fatal("HEAD returned body")
		}
	}
	if r := callAPI(h, "GET", "/api/v1/auth/me", "", ownerToken); r.Code != 200 || !containsText(r.Body.String(), `"avatar_url":"`+path+`"`) {
		t.Fatalf("current account avatar: %d %s", r.Code, r.Body.String())
	}
	var login string
	if err := pool.QueryRow(t.Context(), `SELECT login FROM users WHERE id=$1`, ownerID).Scan(&login); err != nil {
		t.Fatal(err)
	}
	if r := callAPI(h, "GET", "/api/v1/users?q="+login, "", peerToken); r.Code != 200 || !containsText(r.Body.String(), `"avatar_url":"`+path+`"`) {
		t.Fatalf("search avatar: %d %s", r.Code, r.Body.String())
	}
	chat := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, peerID), ownerToken)
	if chat.Code != 201 {
		t.Fatalf("chat: %d %s", chat.Code, chat.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(chat.Body.Bytes(), &created)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM chats WHERE id=$1`, created.ID) })
	if r := callAPI(h, "GET", "/api/v1/chats", "", peerToken); r.Code != 200 || !containsText(r.Body.String(), `"avatar_url":"`+path+`"`) {
		t.Fatalf("direct chat list avatar: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "GET", "/api/v1/chats/"+created.ID, "", peerToken); r.Code != 200 || !containsText(r.Body.String(), `"avatar_url":"`+path+`"`) {
		t.Fatalf("direct chat avatar: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "GET", "/api/v1/chats/"+created.ID+"/members", "", peerToken); r.Code != 200 || !containsText(r.Body.String(), `"avatar_url":"`+path+`"`) {
		t.Fatalf("member avatar: %d %s", r.Code, r.Body.String())
	}
	second := put(ownerToken, "image/jpeg", jpegBytes.Bytes())
	if second.Code != 200 {
		t.Fatalf("replace JPEG: %d %s", second.Code, second.Body.String())
	}
	var secondKey string
	if err := pool.QueryRow(t.Context(), `SELECT avatar_key FROM users WHERE id=$1`, ownerID).Scan(&secondKey); err != nil || secondKey == firstKey {
		t.Fatalf("new object key=%q err=%v", secondKey, err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), secondKey) })
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM avatar_gc WHERE object_key IN ($1,$2)`, firstKey, secondKey)
	})
	if r := callAPI(h, "GET", path, "", ""); r.Code != 200 || r.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(r.Body.Bytes(), jpegBytes.Bytes()) {
		t.Fatalf("replaced public avatar: %d %s", r.Code, r.Body.String())
	}
	var queued bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM avatar_gc WHERE object_key=$1)`, firstKey).Scan(&queued); err != nil || !queued {
		t.Fatalf("old avatar not queued for deletion: %v %v", queued, err)
	}
	if r := callAPI(h, "DELETE", "/api/v1/users/me/avatar", "", ""); r.Code != 401 {
		t.Fatalf("anonymous delete: %d", r.Code)
	}
	if r := callAPI(h, "DELETE", "/api/v1/users/me/avatar", "", ownerToken); r.Code != 204 {
		t.Fatalf("delete avatar: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "GET", path, "", ""); r.Code != 404 {
		t.Fatalf("deleted avatar exposed: %d", r.Code)
	}
	if r := callAPI(h, "GET", "/api/v1/auth/me", "", ownerToken); r.Code != 200 || containsText(r.Body.String(), `"avatar_url"`) {
		t.Fatalf("deleted avatar remains in profile: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "DELETE", "/api/v1/users/me/avatar", "", ownerToken); r.Code != 204 {
		t.Fatalf("repeat delete: %d", r.Code)
	}
}

func TestAvatarCountsTowardUserStorageQuota(t *testing.T) {
	endpoint := testS3Endpoint(t)
	pool := testDatabase(t)
	ownerID, token := seedSession(t, pool, "user")
	store := objects.New(config.Config{S3Endpoint: endpoint, S3Region: "us-east-1", S3Bucket: "familychat-test", S3AccessKey: "familychat_test", S3SecretKey: "familychat_test_secret", S3UsePathStyle: true})
	var avatar bytes.Buffer
	if err := png.Encode(&avatar, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	quota := int64(avatar.Len() + 1)
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, Objects: store, UserQuotaBytes: quota})
	upload := func(body []byte) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("PUT", "/api/v1/users/me/avatar", bytes.NewReader(body))
		r.Header.Set("Content-Type", "image/png")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if r := upload(append(avatar.Bytes(), 0, 0)); r.Code != 413 {
		t.Fatalf("avatar above user quota: %d %s", r.Code, r.Body.String())
	}
	if r := upload(avatar.Bytes()); r.Code != 200 {
		t.Fatalf("avatar at user quota: %d %s", r.Code, r.Body.String())
	}
	var key string
	if err := pool.QueryRow(t.Context(), `SELECT avatar_key FROM users WHERE id=$1`, ownerID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), key) })
	chat := callAPI(h, "POST", "/api/v1/chats", `{"title":"quota"}`, token)
	if chat.Code != 201 {
		t.Fatalf("chat: %d %s", chat.Code, chat.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(chat.Body.Bytes(), &created)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM chats WHERE id=$1`, created.ID) })
	file := httptest.NewRequest("POST", "/api/v1/chats/"+created.ID+"/files", strings.NewReader("xy"))
	file.Header.Set("Authorization", "Bearer "+token)
	file.Header.Set("X-File-Name", "file.txt")
	file.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, file)
	if w.Code != 413 {
		t.Fatalf("chat file exceeded quota including avatar: %d %s", w.Code, w.Body.String())
	}
}
