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

func TestPushShowsCurrentSenderNameWithLoginFallback(t *testing.T) {
	pool := testDatabase(t)
	caller, callerToken := seedSession(t, pool, "user")
	callee, calleeToken := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}, PushEnabled: true})
	deviceID := "00000000-0000-4000-8000-000000000071"
	serverID := "00000000-0000-4000-8000-000000000072"
	registration := fmt.Sprintf(`{"client_server_id":%q,"alert_token":%q,"voip_token":%q}`, serverID, strings.Repeat("a", 64), strings.Repeat("b", 64))
	if r := callAPI(h, "PUT", "/api/v1/push/devices/"+deviceID, registration, calleeToken); r.Code != 204 {
		t.Fatalf("register: %d %s", r.Code, r.Body.String())
	}
	t.Cleanup(func() {
		if r := callAPI(h, "DELETE", "/api/v1/push/devices/"+deviceID, "", calleeToken); r.Code != 204 {
			t.Errorf("remove test device and queued pushes: %d %s", r.Code, r.Body.String())
		}
	})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, callee), callerToken)
	if r.Code != 201 {
		t.Fatalf("chat: %d %s", r.Code, r.Body.String())
	}
	var chat struct{ ID string }
	if err := json.Unmarshal(r.Body.Bytes(), &chat); err != nil {
		t.Fatal(err)
	}
	var login string
	if err := pool.QueryRow(t.Context(), `SELECT login FROM users WHERE id=$1`, caller).Scan(&login); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct{ name, want string }{
		{"Евгений 👋", "Евгений 👋"},
		{"Женя", "Женя"},
		{"", login},
	} {
		t.Run(fmt.Sprintf("profile_%d", i), func(t *testing.T) {
			profile := callAPI(h, "PATCH", "/api/v1/users/me", fmt.Sprintf(`{"display_name":%q}`, tc.name), callerToken)
			if profile.Code != 200 {
				t.Fatalf("profile: %d %s", profile.Code, profile.Body.String())
			}
			message := fmt.Sprintf(`{"client_message_id":"00000000-0000-4000-8000-%012d","text":"private message content"}`, 80+i)
			if r := callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/messages", message, callerToken); r.Code != 201 {
				t.Fatalf("message: %d %s", r.Code, r.Body.String())
			}
			var payload []byte
			if err := pool.QueryRow(t.Context(), `SELECT j.payload FROM push_jobs j JOIN push_devices d ON d.id=j.device_id WHERE d.user_id=$1 AND j.kind='alert' ORDER BY j.id DESC LIMIT 1`, callee).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var alert struct {
				APS struct{ Alert struct{ Title, Body string } } `json:"aps"`
			}
			if err := json.Unmarshal(payload, &alert); err != nil {
				t.Fatal(err)
			}
			if alert.APS.Alert.Title != tc.want || alert.APS.Alert.Body != "Новое сообщение" || strings.Contains(string(payload), "private message content") {
				t.Errorf("message push should name %q without message content: %s", tc.want, payload)
			}
			r := callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/calls", `{"kind":"audio"}`, callerToken)
			if r.Code != 201 {
				t.Fatalf("call: %d %s", r.Code, r.Body.String())
			}
			var call struct{ ID string }
			if err := json.Unmarshal(r.Body.Bytes(), &call); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(t.Context(), `SELECT j.payload FROM push_jobs j JOIN push_devices d ON d.id=j.device_id WHERE d.user_id=$1 AND j.kind='voip' ORDER BY j.id DESC LIMIT 1`, callee).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var voip struct {
				CallerName string `json:"caller_name"`
			}
			if err := json.Unmarshal(payload, &voip); err != nil {
				t.Fatal(err)
			}
			if voip.CallerName != tc.want {
				t.Errorf("call push name = %q, want %q", voip.CallerName, tc.want)
			}
			if r := callAPI(h, "POST", "/api/v1/calls/"+call.ID+"/reject", "", calleeToken); r.Code != 200 {
				t.Fatalf("reject: %d %s", r.Code, r.Body.String())
			}
		})
	}
}
