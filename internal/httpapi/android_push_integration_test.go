package httpapi

import (
	"encoding/json"
	"familychat/server/internal/auth"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAndroidPushRegistrationJobsAndTokenRotation(t *testing.T) {
	db := testDatabase(t)
	_, at := seedSession(t, db, "user")
	b, bt := seedSession(t, db, "user")
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, PushEnabled: true})
	device := "00000000-0000-4000-8000-000000001661"
	server := "00000000-0000-4000-8000-000000001662"
	path := "/api/v1/push/devices/" + device
	registration := fmt.Sprintf(`{"client_server_id":%q,"platform":"android","fcm_token":"Opaque:FCM-Case"}`, server)
	if r := callAPI(h, "PUT", path, registration, bt); r.Code != 204 {
		t.Fatalf("register %d %s", r.Code, r.Body)
	}
	var token string
	if err := db.QueryRow(t.Context(), `SELECT fcm_token FROM push_devices WHERE user_id=$1 AND device_id=$2`, b, device).Scan(&token); err != nil || token != "Opaque:FCM-Case" {
		t.Fatalf("token case %q %v", token, err)
	}
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, b), at)
	var chat struct{ ID string }
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	if chat.ID == "" {
		t.Fatalf("chat %s", r.Body)
	}
	r = callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/messages", `{"client_message_id":"00000000-0000-4000-8000-000000001663","text":"private content"}`, at)
	if r.Code != 201 {
		t.Fatalf("message %d %s", r.Code, r.Body)
	}
	var kind string
	var payload []byte
	if err := db.QueryRow(t.Context(), `SELECT j.kind,j.payload FROM push_jobs j JOIN push_devices d ON d.id=j.device_id WHERE d.user_id=$1 ORDER BY j.id DESC LIMIT 1`, b).Scan(&kind, &payload); err != nil {
		t.Fatal(err)
	}
	if kind != "fcm_message" || strings.Contains(string(payload), "private content") || !strings.Contains(string(payload), server) {
		t.Fatalf("bad message push kind %q payload %s", kind, payload)
	}
	if r := callAPI(h, "PUT", path, registration, bt); r.Code != 204 {
		t.Fatalf("same registration %d", r.Code)
	}
	var count int
	_ = db.QueryRow(t.Context(), `SELECT count(*) FROM push_jobs j JOIN push_devices d ON d.id=j.device_id WHERE d.user_id=$1`, b).Scan(&count)
	if count != 1 {
		t.Fatalf("idempotent registration lost pending job: %d", count)
	}
	r = callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/calls", `{"kind":"audio"}`, at)
	if r.Code != 201 {
		t.Fatalf("call %d %s", r.Code, r.Body)
	}
	var call struct{ ID string }
	_ = json.Unmarshal(r.Body.Bytes(), &call)
	r = callAPI(h, "POST", "/api/v1/calls/"+call.ID+"/accept", "", bt)
	if r.Code != 200 {
		t.Fatalf("accept %d %s", r.Code, r.Body)
	}
	_ = db.QueryRow(t.Context(), `SELECT count(*) FROM push_jobs j JOIN push_devices d ON d.id=j.device_id WHERE d.user_id=$1 AND j.kind='fcm_call_state'`, b).Scan(&count)
	if count != 1 {
		t.Fatalf("missing multi-device cancellation job: %d", count)
	}
	registration = strings.ReplaceAll(registration, "Opaque:FCM-Case", "New:FCM-Token")
	if r := callAPI(h, "PUT", path, registration, bt); r.Code != 204 {
		t.Fatalf("rotation %d", r.Code)
	}
	_ = db.QueryRow(t.Context(), `SELECT count(*) FROM push_jobs j JOIN push_devices d ON d.id=j.device_id WHERE d.user_id=$1`, b).Scan(&count)
	if count != 0 {
		t.Fatalf("rotation retains old jobs: %d", count)
	}
}
