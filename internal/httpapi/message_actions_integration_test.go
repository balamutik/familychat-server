package httpapi

import (
	"encoding/json"
	"familychat/server/internal/auth"
	"familychat/server/internal/contentcrypto"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMessageActions(t *testing.T) {
	db := testDatabase(t)
	_, a := seedSession(t, db, "user")
	bid, b := seedSession(t, db, "user")
	_, outsider := seedSession(t, db, "user")
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, bid), a)
	var chat struct{ ID string }
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	base := "/api/v1/chats/" + chat.ID
	body := `{"client_message_id":"00000000-0000-4000-8000-000000000011","text":"original"}`
	r = callAPI(h, "POST", base+"/messages", body, a)
	var original messageView
	_ = json.Unmarshal(r.Body.Bytes(), &original)
	path := base + "/messages/" + original.ID
	for _, token := range []string{b, outsider} {
		for _, method := range []string{"PATCH", "DELETE"} {
			r = callAPI(h, method, path, `{"text":"forbidden"}`, token)
			if r.Code != 404 {
				t.Fatalf("nonowner %s: %d %s", method, r.Code, r.Body.String())
			}
		}
	}
	r = callAPI(h, "PATCH", path, `{"text":"edited"}`, a)
	if r.Code != 200 {
		t.Fatalf("edit: %d %s", r.Code, r.Body.String())
	}
	var edited messageView
	_ = json.Unmarshal(r.Body.Bytes(), &edited)
	if edited.Text != "edited" || edited.ID != original.ID || edited.Seq != original.Seq || edited.ClientMessageID != original.ClientMessageID || edited.CreatedAt != original.CreatedAt {
		t.Fatalf("identity changed: %+v", edited)
	}
	if r = callAPI(h, "GET", path, "", b); r.Code != 200 || !containsText(r.Body.String(), "edited") {
		t.Fatalf("get: %d %s", r.Code, r.Body.String())
	}
	if r = callAPI(h, "DELETE", path+"?scope=self", "", b); r.Code != 204 {
		t.Fatalf("hide: %d %s", r.Code, r.Body.String())
	}
	if r = callAPI(h, "GET", path, "", b); r.Code != 404 {
		t.Fatalf("hidden get: %d", r.Code)
	}
	if r = callAPI(h, "GET", path, "", a); r.Code != 200 {
		t.Fatalf("peer hidden globally: %d", r.Code)
	}
	for _, token := range []string{a, b} {
		if token == a {
			for i := 0; i < 2; i++ {
				if r = callAPI(h, "DELETE", path+"?scope=everyone", "", a); r.Code != 204 {
					t.Fatalf("delete: %d %s", r.Code, r.Body.String())
				}
			}
		}
		for _, suffix := range []string{"/messages", "/messages?after=0", "/search?q=edited"} {
			r = callAPI(h, "GET", base+suffix, "", token)
			if r.Code != 200 || containsText(r.Body.String(), original.ID) {
				t.Fatalf("visible tombstone %s: %d %s", suffix, r.Code, r.Body.String())
			}
		}
	}
	if r = callAPI(h, "POST", base+"/messages", body, a); r.Code != 409 {
		t.Fatalf("resurrection: %d %s", r.Code, r.Body.String())
	}
	// Hiding one's own message must also survive an outbox retry.
	secondBody := `{"client_message_id":"00000000-0000-4000-8000-000000000014","text":"hidden own"}`
	r = callAPI(h, "POST", base+"/messages", secondBody, a)
	var own messageView
	_ = json.Unmarshal(r.Body.Bytes(), &own)
	r = callAPI(h, "DELETE", base+"/messages/"+own.ID+"?scope=self", "", a)
	if r.Code != 204 {
		t.Fatalf("hide own: %d", r.Code)
	}
	if r = callAPI(h, "POST", base+"/messages", secondBody, a); r.Code != 409 {
		t.Fatalf("hidden outbox resurrection: %d %s", r.Code, r.Body.String())
	}
	var updates, deletes, hides int
	err := db.QueryRow(t.Context(), `SELECT count(*) FILTER(WHERE kind='message_updated'),count(*) FILTER(WHERE kind='message_deleted'),count(*) FILTER(WHERE kind='message_hidden') FROM events WHERE chat_id=$1`, chat.ID).Scan(&updates, &deletes, &hides)
	if err != nil || updates != 1 || deletes != 1 || hides != 2 {
		t.Fatalf("events %d %d %d: %v", updates, deletes, hides, err)
	}
}

func TestMessageDeletionPreservesAttachmentHistoryAndBlocksDownloads(t *testing.T) {
	db := testDatabase(t)
	aid, a := seedSession(t, db, "user")
	bid, b := seedSession(t, db, "user")
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, bid), a)
	var chat chatView
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	base := "/api/v1/chats/" + chat.ID
	var fileID string
	if err := db.QueryRow(t.Context(), `INSERT INTO attachments(chat_id,uploader_id,object_key,filename,content_type,size_bytes,preview_state) VALUES($1,$2,gen_random_uuid()::text,'photo.jpg','image/jpeg',12,'unsupported') RETURNING id::text`, chat.ID, aid).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if r = callAPI(h, "GET", "/api/v1/files/"+fileID, "", a); r.Code != 200 {
		t.Fatalf("upload preview: %d %s", r.Code, r.Body.String())
	}
	body := fmt.Sprintf(`{"client_message_id":"00000000-0000-4000-8000-000000000012","text":"attachment","attachment_ids":[%q]}`, fileID)
	r = callAPI(h, "POST", base+"/messages", body, a)
	var message messageView
	_ = json.Unmarshal(r.Body.Bytes(), &message)
	path := base + "/messages/" + message.ID
	if r = callAPI(h, "PATCH", path, `{"text":"new caption"}`, a); r.Code != 200 || !containsText(r.Body.String(), fileID) {
		t.Fatalf("edit attachment: %d %s", r.Code, r.Body.String())
	}
	if r = callAPI(h, "PATCH", path, `{"text":""}`, a); r.Code != 200 || !containsText(r.Body.String(), fileID) {
		t.Fatalf("clear attachment caption: %d %s", r.Code, r.Body.String())
	}
	if r = callAPI(h, "PATCH", path, `{"text":"new caption"}`, a); r.Code != 200 {
		t.Fatalf("restore caption: %d", r.Code)
	}
	if r = callAPI(h, "DELETE", path+"?scope=self", "", b); r.Code != 204 {
		t.Fatalf("hide: %d", r.Code)
	}
	if r = callAPI(h, "GET", "/api/v1/files/"+fileID, "", b); r.Code != 404 {
		t.Fatalf("hidden attachment: %d", r.Code)
	}
	if r = callAPI(h, "GET", "/api/v1/files/"+fileID, "", a); r.Code != 200 {
		t.Fatalf("peer file hidden: %d", r.Code)
	}
	for _, suffix := range []string{"", "/messages", "/search?q=caption"} {
		r = callAPI(h, "GET", base+suffix, "", b)
		if r.Code != 200 || containsText(r.Body.String(), "new caption") {
			t.Fatalf("hidden content %s: %d %s", suffix, r.Code, r.Body.String())
		}
	}
	if r = callAPI(h, "DELETE", path+"?scope=everyone", "", a); r.Code != 204 {
		t.Fatalf("delete: %d", r.Code)
	}
	for _, token := range []string{a, b} {
		if r = callAPI(h, "GET", "/api/v1/files/"+fileID, "", token); r.Code != 404 {
			t.Fatalf("deleted attachment download: %d", r.Code)
		}
	}
	var count int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM message_attachments WHERE message_id=$1`, message.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("attachment links lost %d %v", count, err)
	}
}

func TestEncryptedMessageEditUsesOriginalClientContext(t *testing.T) {
	db := testDatabase(t)
	aid, a := seedSession(t, db, "user")
	bid, _ := seedSession(t, db, "user")
	key, err := contentcrypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, ContentKey: key})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, bid), a)
	var chat chatView
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	client := "00000000-0000-4000-8000-000000000013"
	context := contentcrypto.MessageContext(chat.ID, aid, client)
	sealed, err := key.EncryptText("original", context)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/chats/" + chat.ID + "/messages"
	r = callAPI(h, "POST", base, fmt.Sprintf(`{"client_message_id":%q,"text":%q,"encrypted":true}`, client, sealed), a)
	var message messageView
	_ = json.Unmarshal(r.Body.Bytes(), &message)
	path := base + "/" + message.ID
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+a)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 426 {
		t.Fatalf("unencrypted GET: %d", w.Code)
	}
	for _, body := range []string{`{"text":"plain"}`, `{"text":"invalid","encrypted":true}`} {
		r = callAPI(h, "PATCH", path, body, a)
		if r.Code != 426 && r.Code != 400 {
			t.Fatalf("invalid encrypted edit: %d", r.Code)
		}
	}
	sealed, err = key.EncryptText("updated", context)
	if err != nil {
		t.Fatal(err)
	}
	r = callAPI(h, "PATCH", path, fmt.Sprintf(`{"text":%q,"encrypted":true}`, sealed), a)
	var edited messageView
	_ = json.Unmarshal(r.Body.Bytes(), &edited)
	if r.Code != 200 || edited.Text != sealed || edited.ClientMessageID != client || !edited.Encrypted {
		t.Fatalf("encrypted edit: %d %s", r.Code, r.Body.String())
	}
}

func TestMessageIDsReconciliationFiltersVisibilityAndBoundsRequests(t *testing.T) {
	db := testDatabase(t)
	_, a := seedSession(t, db, "user")
	bid, b := seedSession(t, db, "user")
	cid, c := seedSession(t, db, "user")
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}})
	create := func(peer string) string {
		r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, peer), a)
		var chat chatView
		_ = json.Unmarshal(r.Body.Bytes(), &chat)
		return "/api/v1/chats/" + chat.ID + "/messages"
	}
	base := create(bid)
	other := create(cid)
	send := func(path string, n int) string {
		r := callAPI(h, "POST", path, fmt.Sprintf(`{"client_message_id":"00000000-0000-4000-8000-%012d","text":"reconcile"}`, n), a)
		var message messageView
		_ = json.Unmarshal(r.Body.Bytes(), &message)
		return message.ID
	}
	visible, hidden, deleted, foreign := send(base, 21), send(base, 22), send(base, 23), send(other, 24)
	callAPI(h, "DELETE", base+"/"+hidden+"?scope=self", "", b)
	callAPI(h, "DELETE", base+"/"+deleted+"?scope=everyone", "", a)
	ids := strings.Join([]string{visible, hidden, deleted, foreign, "00000000-0000-4000-8000-000000000099"}, ",")
	r := callAPI(h, "GET", base+"?ids="+ids, "", b)
	var response struct {
		Messages []messageView `json:"messages"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &response)
	if r.Code != 200 || len(response.Messages) != 1 || response.Messages[0].ID != visible {
		t.Fatalf("reconcile: %d %s", r.Code, r.Body.String())
	}
	if r = callAPI(h, "GET", base+"?ids="+visible, "", c); r.Code != 404 {
		t.Fatalf("foreign member: %d", r.Code)
	}
	for _, query := range []string{"ids=bad", "ids=", "ids=" + visible + "&after=0", "ids=" + visible + "&before=100", "ids=" + strings.Repeat(visible+",", 100) + visible} {
		if r = callAPI(h, "GET", base+"?"+query, "", a); r.Code != 400 {
			t.Fatalf("invalid ids %q: %d", query, r.Code)
		}
	}
	// IDs mode ignores the ordinary page limit, including a client-provided limit.
	expected := []string{visible}
	for n := 30; n < 90; n++ {
		expected = append(expected, send(base, n))
	}
	r = callAPI(h, "GET", base+"?ids="+strings.Join(expected, ",")+"&limit=1", "", a)
	_ = json.Unmarshal(r.Body.Bytes(), &response)
	if r.Code != 200 || len(response.Messages) != len(expected) {
		t.Fatalf("ids truncated: %d count=%d %s", r.Code, len(response.Messages), r.Body.String())
	}
}
