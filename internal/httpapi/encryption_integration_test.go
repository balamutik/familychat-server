package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/config"
	"familychat/server/internal/contentcrypto"
	"familychat/server/internal/objects"
)

func TestEncryptedTransportKeepsMessagesAndFilesOpaque(t *testing.T) {
	endpoint := testS3Endpoint(t)
	db := testDatabase(t)
	sender, st := seedSession(t, db, "user")
	recipient, rt := seedSession(t, db, "user")
	_, outsider := seedSession(t, db, "user")
	key, err := contentcrypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	store := objects.New(config.Config{S3Endpoint: endpoint, S3Region: "us-east-1", S3Bucket: "familychat-test", S3AccessKey: "familychat_test", S3SecretKey: "familychat_test_secret", S3UsePathStyle: true})
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, Objects: store, ContentKey: key})
	if r := callAPI(h, "GET", "/api/v1/encryption/key", "", ""); r.Code != 401 {
		t.Fatal("keys exposed anonymously")
	}
	r := callAPI(h, "GET", "/api/v1/encryption/key", "", st)
	if r.Code != 200 || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("key endpoint: %d", r.Code)
	}
	var deliveredKey contentcrypto.Key
	if json.Unmarshal(r.Body.Bytes(), &deliveredKey) != nil || deliveredKey.Validate() != nil || deliveredKey.ID != key.ID {
		t.Fatal("wrong key")
	}
	r = callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, recipient), st)
	var chat chatView
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &chat) != nil {
		t.Fatalf("chat: %s", r.Body.String())
	}
	client := "00000000-0000-4000-8000-000000000abc"
	context := contentcrypto.MessageContext(chat.ID, sender, client)
	plaintext := "Секретное семейное сообщение 🔐"
	sealed, err := key.EncryptText(plaintext, context)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/chats/" + chat.ID + "/messages"
	if r := callAPI(h, "POST", path, fmt.Sprintf(`{"client_message_id":%q,"text":%q}`, client, plaintext), st); r.Code != 426 {
		t.Fatalf("accepted plaintext: %d", r.Code)
	}
	send := func(text string) *httptest.ResponseRecorder {
		return callAPI(h, "POST", path, fmt.Sprintf(`{"client_message_id":%q,"text":%q,"encrypted":true}`, client, text), st)
	}
	r = send(sealed)
	if r.Code != 201 {
		t.Fatalf("encrypted send: %d %s", r.Code, r.Body.String())
	}
	var message messageView
	_ = json.Unmarshal(r.Body.Bytes(), &message)
	var stored string
	var encrypted bool
	if err = db.QueryRow(t.Context(), `SELECT body,encrypted FROM messages WHERE id=$1`, message.ID).Scan(&stored, &encrypted); err != nil || stored != sealed || !encrypted {
		t.Fatal("database did not preserve ciphertext")
	}
	retry, _ := key.EncryptText(plaintext, context)
	if r = send(retry); r.Code != 200 || !bytes.Contains(r.Body.Bytes(), []byte(sealed)) {
		t.Fatalf("randomized retry failed: %d", r.Code)
	}
	for _, route := range []string{path, "/api/v1/chats"} {
		r = callAPI(h, "GET", route, "", rt)
		if r.Code != 200 || bytes.Contains(r.Body.Bytes(), []byte(plaintext)) || !bytes.Contains(r.Body.Bytes(), []byte(sealed)) {
			t.Fatalf("ciphertext read %s: %d %s", route, r.Code, r.Body.String())
		}
	}
	if r = callAPI(h, "GET", path, "", outsider); r.Code != 404 {
		t.Fatal("outsider accessed chat")
	}
	if r = callAPI(h, "GET", "/api/v1/chats/"+chat.ID+"/search?q=secret", "", rt); r.Code != 410 {
		t.Fatal("server still searches encrypted history")
	}
	var file bytes.Buffer
	plainFile := bytes.Repeat([]byte("private attachment"), 70000)
	if err = key.Encrypt(&file, bytes.NewReader(plainFile), int64(len(plainFile)), "file:"+chat.ID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/chats/"+chat.ID+"/files", bytes.NewReader(file.Bytes()))
	req.Header.Set("Authorization", "Bearer "+st)
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("X-File-Name", "private.jpg")
	req.Header.Set("X-Content-Encryption", "fc1")
	req.Header.Set("X-Plaintext-Size", fmt.Sprint(len(plainFile)))
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	var metadata fileMeta
	if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &metadata) != nil {
		t.Fatalf("upload: %d %s", response.Code, response.Body.String())
	}
	if metadata.PreviewState != "unsupported" {
		t.Fatal("encrypted original must remain usable without a preview")
	}
	var objectKey string
	if err = db.QueryRow(t.Context(), `SELECT object_key FROM attachments WHERE id=$1`, metadata.ID).Scan(&objectKey); err != nil {
		t.Fatal(err)
	}
	defer store.Delete(t.Context(), objectKey)
	obj, err := store.Get(t.Context(), objectKey, "")
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := io.ReadAll(obj.Body)
	obj.Body.Close()
	if err != nil || !bytes.Equal(onDisk, file.Bytes()) || bytes.Contains(onDisk, plainFile) {
		t.Fatal("storage is not opaque")
	}
	filePath := "/api/v1/files/" + metadata.ID
	preview, _ := key.EncryptText("preview bytes", "preview:"+chat.ID)
	previewBytes, _ := base64.StdEncoding.DecodeString(preview)
	limited := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, Objects: store, ContentKey: key, UserQuotaBytes: int64(file.Len() + len(previewBytes) - 1)})
	if denied := callAPI(limited, "PUT", filePath+"/preview", string(previewBytes), st); denied.Code != 413 {
		t.Fatalf("preview bypassed quota: %d", denied.Code)
	}
	r = callAPI(h, "PUT", filePath+"/preview", string(previewBytes), st)
	if r.Code != 200 {
		t.Fatalf("preview upload: %d %s", r.Code, r.Body.String())
	}
	r = callAPI(h, "GET", filePath+"/preview", "", st)
	if r.Code != 200 || !bytes.Equal(r.Body.Bytes(), previewBytes) {
		t.Fatal("preview is not opaque")
	}
	if r = callAPI(h, "GET", filePath+"/content", "", outsider); r.Code != 404 {
		t.Fatal("outsider downloaded attachment")
	}
	r = callAPI(h, "GET", filePath+"/content", "", st)
	if r.Code != 200 || !bytes.Equal(r.Body.Bytes(), file.Bytes()) || r.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatal("encrypted download mismatch")
	}
	var jobs int
	if err = db.QueryRow(t.Context(), `SELECT count(*) FROM preview_jobs WHERE attachment_id=$1`, metadata.ID).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatal("server queued plaintext preview generation")
	}
}
