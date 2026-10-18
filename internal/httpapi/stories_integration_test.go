package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/contentcrypto"
	"familychat/server/internal/objects"
)

type storyTestStore struct {
	mu     sync.Mutex
	data   map[string][]byte
	putErr error
}

func (s *storyTestStore) Ping(context.Context) error { return nil }
func (s *storyTestStore) Put(_ context.Context, k string, r io.Reader, size int64, _ string) error {
	if s.putErr != nil {
		return s.putErr
	}
	b, e := io.ReadAll(r)
	if e != nil {
		return e
	}
	if int64(len(b)) != size {
		return errors.New("wrong length")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[k] = b
	return nil
}
func (s *storyTestStore) Head(_ context.Context, k string) (objects.Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.data[k]
	if !ok {
		return objects.Metadata{}, errors.New("missing")
	}
	return objects.Metadata{Size: int64(len(b))}, nil
}
func (s *storyTestStore) Get(_ context.Context, k, rg string) (objects.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.data[k]
	if !ok {
		return objects.Object{}, errors.New("missing")
	}
	if rg != "" {
		start, end, e := parseByteRange(rg, int64(len(b)))
		if e != nil {
			return objects.Object{}, e
		}
		b = b[start : end+1]
	}
	return objects.Object{Body: io.NopCloser(bytes.NewReader(b)), Metadata: objects.Metadata{Size: int64(len(b))}}, nil
}
func (s *storyTestStore) Delete(_ context.Context, k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, k)
	return nil
}

type testStory struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Login       string    `json:"login"`
	DisplayName string    `json:"display_name"`
	AvatarURL   string    `json:"avatar_url"`
	Caption     string    `json:"caption"`
	ContentType string    `json:"content_type"`
	Filename    string    `json:"filename"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Viewed      bool      `json:"viewed"`
	Encrypted   bool      `json:"encrypted"`
}

func TestEncryptedStoriesAudienceViewsDeleteAndExpiry(t *testing.T) {
	db := testDatabase(t)
	aid, a := seedSession(t, db, "user")
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM stories WHERE user_id=$1`, aid) })
	bid, b := seedSession(t, db, "user")
	_, c := seedSession(t, db, "user")
	key, err := contentcrypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	store := &storyTestStore{data: map[string][]byte{}}
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, Objects: store, ContentKey: key})
	if _, err = db.Exec(t.Context(), `UPDATE users SET display_name='Story Author',avatar_key='story-avatar-'||id::text WHERE id=$1`, aid); err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("opaque photo bytes")
	var sealed bytes.Buffer
	if err = key.Encrypt(&sealed, bytes.NewReader(plaintext), int64(len(plaintext)), "story"); err != nil {
		t.Fatal(err)
	}
	caption, err := key.EncryptText("My story", "story-caption")
	if err != nil {
		t.Fatal(err)
	}
	upload := func(token, ctype, cap string, encrypted bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/stories", bytes.NewReader(sealed.Bytes()))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", ctype)
		r.Header.Set("X-File-Name", "photo.jpg")
		r.Header.Set("X-Story-Caption", url.PathEscape(cap))
		if encrypted {
			r.Header.Set("X-Content-Encryption", "fc1")
			r.Header.Set("X-Plaintext-Size", strconv.Itoa(len(plaintext)))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if r := upload(a, "image/jpeg", caption, false); r.Code != 426 {
		t.Fatalf("plaintext accepted %d %s", r.Code, r.Body.String())
	}
	if r := upload(a, "text/html", caption, true); r.Code != 400 {
		t.Fatalf("nonmedia accepted %d", r.Code)
	}
	if r := upload(a, "image/jpeg", "plain caption", true); r.Code != 400 {
		t.Fatalf("plain caption accepted %d", r.Code)
	}
	r := upload(a, "image/jpeg", caption, true)
	var story testStory
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &story) != nil || story.ID == "" {
		t.Fatalf("create %d %s", r.Code, r.Body.String())
	}
	if story.UserID != aid || story.Caption != caption || story.ContentType != "image/jpeg" || story.Size != int64(len(plaintext)) || !story.Encrypted || story.DisplayName != "Story Author" || story.AvatarURL != avatarURL(aid) || story.ExpiresAt.Sub(story.CreatedAt) != 24*time.Hour {
		t.Fatalf("metadata %+v", story)
	}
	path := "/api/v1/stories/" + story.ID
	list := func(token string) []testStory {
		t.Helper()
		r := callAPI(h, "GET", "/api/v1/stories", "", token)
		var page struct {
			Stories []testStory `json:"stories"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &page) != nil {
			t.Fatalf("list %d %s", r.Code, r.Body.String())
		}
		return page.Stories
	}
	if got := list(b); len(got) != 1 || got[0].Viewed {
		t.Fatalf("unviewed %+v", got)
	}
	if r = callAPI(h, "PUT", path+"/view", "", b); r.Code != 204 {
		t.Fatalf("view %d", r.Code)
	}
	if got := list(b); len(got) != 1 || !got[0].Viewed {
		t.Fatalf("view not persisted %+v", got)
	}
	if got := list(c); len(got) != 1 || got[0].Viewed {
		t.Fatal("view leaked to other account")
	}
	req := httptest.NewRequest("GET", path+"/content", nil)
	req.Header.Set("Authorization", "Bearer "+b)
	req.Header.Set("X-Content-Encryption", "fc1")
	req.Header.Set("Range", "bytes=0-9")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), sealed.Bytes()[:10]) {
		t.Fatalf("range %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest("GET", path+"/content", nil)
	req.Header.Set("Authorization", "Bearer "+b)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 426 {
		t.Fatalf("missing encryption header %d", w.Code)
	}
	if r = callAPI(h, "DELETE", path, "", b); r.Code != 404 {
		t.Fatalf("nonowner delete %d", r.Code)
	}
	if r = callAPI(h, "PUT", "/api/v1/users/"+aid+"/block", `{"blocked":true}`, b); r.Code != 204 {
		t.Fatalf("block %d", r.Code)
	}
	if got := list(b); len(got) != 0 {
		t.Fatal("blocked author visible")
	}
	for _, suffix := range []string{"", "/content", "/view"} {
		method := "GET"
		if suffix == "/view" {
			method = "PUT"
		}
		if r = callAPI(h, method, path+suffix, "", b); r.Code != 404 {
			t.Fatalf("block bypass %s %d", suffix, r.Code)
		}
	}
	if got := list(c); len(got) != 1 {
		t.Fatal("unrelated audience affected")
	}
	if _, err = db.Exec(t.Context(), `DELETE FROM user_blocks WHERE source_user_id=$1 AND target_user_id=$2`, bid, aid); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(t.Context(), `INSERT INTO user_blocks(source_user_id,target_user_id) VALUES($1,$2)`, aid, bid); err != nil {
		t.Fatal(err)
	}
	if got := list(b); len(got) != 0 {
		t.Fatal("reverse block bypass")
	}
	if _, err = db.Exec(t.Context(), `UPDATE users SET disabled=true WHERE id=$1`, aid); err != nil {
		t.Fatal(err)
	}
	if got := list(c); len(got) != 0 {
		t.Fatal("disabled author visible")
	}
	if _, err = db.Exec(t.Context(), `UPDATE users SET disabled=false WHERE id=$1`, aid); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(t.Context(), `UPDATE stories SET expires_at=now()-interval '1 second' WHERE id=$1`, story.ID); err != nil {
		t.Fatal(err)
	}
	if got := list(c); len(got) != 0 {
		t.Fatal("expired visible")
	}
	if r = callAPI(h, "GET", path+"/content", "", c); r.Code != 404 {
		t.Fatalf("expired download %d", r.Code)
	}
	for i := 0; i < 2; i++ {
		if r = callAPI(h, "DELETE", path, "", a); r.Code != 204 {
			t.Fatalf("owner delete %d %s", r.Code, r.Body.String())
		}
	}
	if r = callAPI(h, "GET", "/api/v1/stories", "", ""); r.Code != 401 {
		t.Fatalf("anonymous stories %d", r.Code)
	}
}

func TestStoriesQuotaIncludesStoredCiphertextAndExistingUploads(t *testing.T) {
	db := testDatabase(t)
	uid, token := seedSession(t, db, "user")
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM stories WHERE user_id=$1`, uid) })
	store := &storyTestStore{data: map[string][]byte{}}
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, Objects: store, UserQuotaBytes: 5})
	upload := func(data string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/stories", bytes.NewBufferString(data))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "video/mp4")
		r.Header.Set("X-File-Name", "clip.mp4")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if r := upload("12345"); r.Code != 201 {
		t.Fatalf("at quota %d %s", r.Code, r.Body.String())
	}
	if r := upload("x"); r.Code != 413 {
		t.Fatalf("quota bypass %d %s", r.Code, r.Body.String())
	}
	var count int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM stories`).Scan(&count); err != nil || count < 1 {
		t.Fatalf("story persistence %d %v", count, err)
	}
	// Chat-file uploads must account for storage occupied by stories too.
	group := callAPI(h, "POST", "/api/v1/chats", `{"title":"quota"}`, token)
	var chat chatView
	_ = json.Unmarshal(group.Body.Bytes(), &chat)
	req := httptest.NewRequest("POST", "/api/v1/chats/"+chat.ID+"/files", bytes.NewBufferString("x"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-File-Name", "file.txt")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 413 {
		t.Fatalf("file quota ignores stories %d %s", w.Code, w.Body.String())
	}

}

func TestFailedStoryUploadReleasesReservation(t *testing.T) {
	db := testDatabase(t)
	uid, token := seedSession(t, db, "user")
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM stories WHERE user_id=$1`, uid) })
	store := &storyTestStore{data: map[string][]byte{}, putErr: errors.New("failed storage")}
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, Objects: store, UserQuotaBytes: 4})
	upload := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/stories", bytes.NewBufferString("data"))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "image/jpeg")
		r.Header.Set("X-File-Name", "photo.jpg")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if r := upload(); r.Code != 502 {
		t.Fatalf("failed upload %d %s", r.Code, r.Body.String())
	}
	var used int64
	if err := db.QueryRow(t.Context(), `SELECT COALESCE(sum(stored_size_bytes),0) FROM stories WHERE user_id=$1 AND purged_at IS NULL`, uid).Scan(&used); err != nil || used != 0 {
		t.Fatalf("failed quota leak %d %v", used, err)
	}
	store.putErr = nil
	if r := upload(); r.Code != 201 {
		t.Fatalf("retry quota %d %s", r.Code, r.Body.String())
	}
}

func TestStoriesRejectMediaAbove50MiBBeforeStorage(t *testing.T) {
	db := testDatabase(t)
	_, token := seedSession(t, db, "user")
	store := &storyTestStore{data: map[string][]byte{}}
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, Objects: store})
	r := httptest.NewRequest("POST", "/api/v1/stories", bytes.NewBufferString("x"))
	r.ContentLength = (50 << 20) + 1
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "video/mp4")
	r.Header.Set("X-File-Name", "clip.mp4")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("oversized story %d %s", w.Code, w.Body.String())
	}
	if len(store.data) != 0 {
		t.Fatal("oversized story reached storage")
	}
}

func TestStoriesFailClosedWhenEncryptionIsEnabledAfterPlainUpload(t *testing.T) {
	db := testDatabase(t)
	uid, token := seedSession(t, db, "user")
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM stories WHERE user_id=$1`, uid) })
	store := &storyTestStore{data: map[string][]byte{}}
	deps := Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, Objects: store}
	plain := New(deps)
	req := httptest.NewRequest("POST", "/api/v1/stories", bytes.NewBufferString("plain"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("X-File-Name", "photo.jpg")
	w := httptest.NewRecorder()
	plain.ServeHTTP(w, req)
	var story testStory
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &story) != nil {
		t.Fatalf("plain compatibility create %d %s", w.Code, w.Body.String())
	}
	key, err := contentcrypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	deps.ContentKey = key
	encrypted := New(deps)
	r := callAPI(encrypted, "GET", "/api/v1/stories", "", token)
	if r.Code != 200 || containsText(r.Body.String(), story.ID) {
		t.Fatalf("plaintext feed fallback %d %s", r.Code, r.Body.String())
	}
	for _, suffix := range []string{"", "/content", "/view"} {
		method := "GET"
		if suffix == "/view" {
			method = "PUT"
		}
		r = callAPI(encrypted, method, "/api/v1/stories/"+story.ID+suffix, "", token)
		if r.Code != 404 {
			t.Fatalf("plaintext fallback %s: %d", suffix, r.Code)
		}
	}
}
