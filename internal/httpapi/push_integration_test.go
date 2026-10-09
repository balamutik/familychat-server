package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"familychat/server/internal/auth"
)

func TestPushRegistrationAndTransactionalJobs(t *testing.T) {
	pool := testDatabase(t)
	_, callerToken := seedSession(t, pool, "user")
	callee, calleeToken := seedSession(t, pool, "user")
	_, outsiderToken := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, PushEnabled: true})
	deviceID := "00000000-0000-4000-8000-000000000061"
	serverID := "00000000-0000-4000-8000-000000000062"
	path := "/api/v1/push/devices/" + deviceID
	body := fmt.Sprintf(`{"client_server_id":%q,"alert_token":%q,"voip_token":%q}`, serverID, strings.Repeat("a", 64), strings.Repeat("b", 64))
	if r := callAPI(h, "PUT", path, body, calleeToken); r.Code != 204 {
		t.Fatalf("register: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "PUT", path, `{"client_server_id":"bad","alert_token":"bad"}`, calleeToken); r.Code != 400 {
		t.Fatalf("invalid: %d", r.Code)
	}
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, callee), callerToken)
	if r.Code != 201 {
		t.Fatalf("chat: %d %s", r.Code, r.Body.String())
	}
	var chat struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &chat)
	r = callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/messages", `{"client_message_id":"00000000-0000-4000-8000-000000000063","text":"private text"}`, callerToken)
	if r.Code != 201 {
		t.Fatalf("message: %d %s", r.Code, r.Body.String())
	}
	var kind string
	var payload []byte
	if err := pool.QueryRow(t.Context(), `SELECT kind,payload FROM push_jobs ORDER BY id DESC LIMIT 1`).Scan(&kind, &payload); err != nil {
		t.Fatal(err)
	}
	if kind != "alert" || !strings.Contains(string(payload), serverID) || strings.Contains(string(payload), "private text") {
		t.Fatalf("alert job: kind=%s payload=%s", kind, payload)
	}
	r = callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/calls", `{"kind":"audio"}`, callerToken)
	if r.Code != 201 {
		t.Fatalf("call: %d %s", r.Code, r.Body.String())
	}
	if err := pool.QueryRow(t.Context(), `SELECT kind,payload FROM push_jobs ORDER BY id DESC LIMIT 1`).Scan(&kind, &payload); err != nil {
		t.Fatal(err)
	}
	if kind != "voip" || !strings.Contains(string(payload), `"caller_name"`) || !strings.Contains(string(payload), serverID) {
		t.Fatalf("voip job: kind=%s payload=%s", kind, payload)
	}
	if r := callAPI(h, "DELETE", path, "", outsiderToken); r.Code != 204 {
		t.Fatalf("outsider delete: %d", r.Code)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM push_devices WHERE user_id=$1`, callee).Scan(&count); err != nil || count != 1 {
		t.Fatalf("device removed by outsider: %d %v", count, err)
	}
	if r := callAPI(h, "DELETE", path, "", calleeToken); r.Code != 204 {
		t.Fatalf("delete: %d", r.Code)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM push_jobs j JOIN push_devices d ON d.id=j.device_id WHERE d.user_id=$1`, callee).Scan(&count); err != nil || count != 0 {
		t.Fatalf("jobs after delete: %d %v", count, err)
	}
}
