package httpapi

import (
	"context"
	"encoding/json"
	"familychat/server/internal/auth"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestInviteLifecycle(t *testing.T) {
	pool := testDatabase(t)
	_, admin := seedSession(t, pool, "admin")
	_, user := seedSession(t, pool, "user")
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: time.Hour}})
	expiry := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	body := fmt.Sprintf(`{"server_name":"Семья","server_url":"https://chat.example","expires_at":%q}`, expiry.Format(time.RFC3339))
	for _, token := range []string{"", user} {
		r := callAPI(h, "POST", "/api/v1/admin/invites", body, token)
		if r.Code != 401 && r.Code != 403 {
			t.Fatalf("unauthorized creation: %d", r.Code)
		}
	}
	r := callAPI(h, "POST", "/api/v1/admin/invites", body, admin)
	if r.Code != 201 {
		t.Fatalf("create: %d %s", r.Code, r.Body.String())
	}
	var v struct {
		ID        string    `json:"id"`
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !v.ExpiresAt.Equal(expiry) {
		t.Fatal("custom expiry not retained")
	}
	token := strings.TrimPrefix(v.URL, "https://chat.example/invite/")
	if len(token) != 64 {
		t.Fatal("invalid invite URL")
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM invitations WHERE id=$1`, v.ID) })
	r = callAPI(h, "GET", "/api/v1/invites/"+token, "", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"server_name":"Семья"`) || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("resolve: %d %s", r.Code, r.Body.String())
	}
	r = callAPI(h, "GET", "/invite/"+token, "", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "Открыть FamilyChat") || !strings.Contains(r.Body.String(), "apps.apple.com") {
		t.Fatalf("landing: %d", r.Code)
	}
	r = callAPI(h, "DELETE", "/api/v1/admin/invites/"+v.ID, "", user)
	if r.Code != 403 {
		t.Fatal("member revoked invite")
	}
	r = callAPI(h, "GET", "/api/v1/admin/invites", "", admin)
	if r.Code != 200 || !strings.Contains(r.Body.String(), v.ID) {
		t.Fatal("missing in admin list")
	}
	if r = callAPI(h, "DELETE", "/api/v1/admin/invites/"+v.ID, "", admin); r.Code != 204 {
		t.Fatal("revoke failed")
	}
	for _, path := range []string{"/api/v1/invites/", "/invite/"} {
		if r = callAPI(h, "GET", path+token, "", ""); r.Code != 410 {
			t.Fatalf("revoked: %d", r.Code)
		}
	}
	_, err := pool.Exec(context.Background(), `UPDATE invitations SET revoked_at=NULL,expires_at=now()-interval '1 second' WHERE id=$1`, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r = callAPI(h, "GET", "/api/v1/invites/"+token, "", ""); r.Code != 410 {
		t.Fatalf("expired: %d", r.Code)
	}
	for _, bad := range []string{strings.Replace(body, expiry.Format(time.RFC3339), time.Now().Add(-time.Hour).Format(time.RFC3339), 1), strings.Replace(body, "https://chat.example", "javascript:alert(1)", 1), strings.Replace(body, "https://chat.example", "https://user:pass@chat.example", 1)} {
		if r = callAPI(h, "POST", "/api/v1/admin/invites", bad, admin); r.Code != 400 {
			t.Fatalf("invalid create: %d", r.Code)
		}
	}
}

func TestInviteEndpointsRequireAdmin(t *testing.T) {
	h := New(Dependencies{})
	if r := callAPI(h, "POST", "/api/v1/admin/invites", `{}`, ""); r.Code != 401 {
		t.Fatalf("expected auth gate, got %d", r.Code)
	}
}
