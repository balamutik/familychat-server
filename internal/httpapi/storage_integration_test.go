package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/config"
	"familychat/server/internal/objects"
)

func TestAdminStorageSettings(t *testing.T) {
	pool := testDatabase(t)
	_, adminToken := seedSession(t, pool, "admin")
	userID, userToken := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, MaxFileBytes: 250 << 20})
	path := "/api/v1/admin/settings/storage"
	if r := callAPI(h, "GET", path, "", userToken); r.Code != 403 {
		t.Fatalf("user access: %d", r.Code)
	}
	if r := callAPI(h, "GET", path, "", ""); r.Code != 401 {
		t.Fatalf("anonymous access: %d", r.Code)
	}
	r := callAPI(h, "GET", path, "", adminToken)
	if r.Code != 200 {
		t.Fatalf("get settings: %d %s", r.Code, r.Body.String())
	}
	var setting struct {
		MaxFileBytes    int64 `json:"max_file_bytes"`
		MaxAllowedBytes int64 `json:"max_allowed_bytes"`
		RetentionDays   int   `json:"retention_days"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &setting); err != nil || setting.MaxAllowedBytes != 250<<20 {
		t.Fatalf("setting=%+v err=%v", setting, err)
	}
	initial := setting
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE settings SET max_file_bytes=$1,retention_days=$2 WHERE singleton=true`, initial.MaxFileBytes, initial.RetentionDays)
	})
	for _, body := range []string{`{"max_file_bytes":0,"retention_days":1}`, `{"max_file_bytes":1024,"retention_days":1}`, `{"max_file_bytes":1572864,"retention_days":1}`, `{"max_file_bytes":262144001,"retention_days":1}`, `{"max_file_bytes":1048576,"retention_days":-1}`, `{"max_file_bytes":1048576,"retention_days":3651}`, `{"max_file_bytes":1048576}`} {
		if r := callAPI(h, "PATCH", path, body, adminToken); r.Code != 400 {
			t.Fatalf("invalid setting %s: %d", body, r.Code)
		}
	}
	r = callAPI(h, "PATCH", path, `{"max_file_bytes":1048576,"retention_days":1}`, adminToken)
	if r.Code != 200 {
		t.Fatalf("save settings: %d %s", r.Code, r.Body.String())
	}
	r = callAPI(h, "GET", path, "", adminToken)
	if err := json.Unmarshal(r.Body.Bytes(), &setting); err != nil || setting.MaxFileBytes != 1048576 || setting.RetentionDays != 1 {
		t.Fatalf("persisted=%+v err=%v", setting, err)
	}
	// A fresh handler reads the database setting rather than process-local state.
	h = New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, MaxFileBytes: 250 << 20})
	r = callAPI(h, "GET", path, "", adminToken)
	if !containsText(r.Body.String(), `"max_file_bytes":1048576`) {
		t.Fatalf("restart persistence: %s", r.Body.String())
	}
	store := objects.New(config.Config{S3Endpoint: "http://127.0.0.1:8338", S3Region: "us-east-1", S3Bucket: "familychat-test", S3AccessKey: "unused", S3SecretKey: "unused", S3UsePathStyle: true})
	h = New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, Objects: store, MaxFileBytes: 250 << 20})
	r = callAPI(h, "POST", "/api/v1/chats", `{"title":"storage test"}`, userToken)
	if r.Code != 201 {
		t.Fatalf("chat: %d", r.Code)
	}
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM chats WHERE id=$1`, chat.ID) })
	pathToChat := "/api/v1/chats/" + chat.ID
	upload := httptest.NewRequest("POST", pathToChat+"/files", strings.NewReader(strings.Repeat("x", (1<<20)+1)))
	upload.Header.Set("Authorization", "Bearer "+userToken)
	upload.Header.Set("X-File-Name", "too-large.txt")
	upload.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, upload)
	if w.Code != 413 {
		t.Fatalf("configured size limit: %d %s", w.Code, w.Body.String())
	}
	var fileID string
	if err := pool.QueryRow(t.Context(), `INSERT INTO attachments(chat_id,uploader_id,object_key,filename,content_type,size_bytes)
		VALUES($1,$2,$3,'old.txt','text/plain',10) RETURNING id::text`, chat.ID, userID, "tests/old-"+chat.ID).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	message := fmt.Sprintf(`{"client_message_id":"00000000-0000-4000-8000-000000000032","text":"Retained message","attachment_ids":[%q]}`, fileID)
	if r := callAPI(h, "POST", pathToChat+"/messages", message, userToken); r.Code != 201 {
		t.Fatalf("message: %d %s", r.Code, r.Body.String())
	}
	if _, err := pool.Exec(t.Context(), `UPDATE attachments SET created_at=now()-interval '2 days' WHERE id=$1`, fileID); err != nil {
		t.Fatal(err)
	}
	if r := callAPI(h, "GET", "/api/v1/files/"+fileID, "", userToken); r.Code != 404 {
		t.Fatalf("expired metadata: %d", r.Code)
	}
	if r := callAPI(h, "GET", "/api/v1/files/"+fileID+"/content", "", userToken); r.Code != 404 {
		t.Fatalf("expired bytes: %d", r.Code)
	}
	if r := callAPI(h, "GET", pathToChat+"/messages", "", userToken); r.Code != 200 || !containsText(r.Body.String(), `"available":false`) {
		t.Fatalf("expired history: %d %s", r.Code, r.Body.String())
	}
	var unattachedID string
	if err := pool.QueryRow(t.Context(), `INSERT INTO attachments(chat_id,uploader_id,object_key,filename,content_type,size_bytes,created_at)
		VALUES($1,$2,$3,'older.txt','text/plain',10,now()-interval '2 days') RETURNING id::text`, chat.ID, userID, "tests/older-"+chat.ID).Scan(&unattachedID); err != nil {
		t.Fatal(err)
	}
	message = fmt.Sprintf(`{"client_message_id":"00000000-0000-4000-8000-000000000033","text":"Too late","attachment_ids":[%q]}`, unattachedID)
	if r := callAPI(h, "POST", pathToChat+"/messages", message, userToken); r.Code != 404 {
		t.Fatalf("expired attachment reused: %d %s", r.Code, r.Body.String())
	}
}
