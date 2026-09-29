package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/database"
)

func TestRegistrationAdminAndRevokedSession(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run auth integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE settings SET registration_enabled=false`); err != nil {
		t.Fatal(err)
	}
	seed := time.Now().UnixNano() % 100000000
	adminLogin := fmt.Sprintf("adm%d", seed)
	userLogin := fmt.Sprintf("usr%d", seed)
	hash, err := auth.HashPassword("admin secure password")
	if err != nil {
		t.Fatal(err)
	}
	var adminID string
	if err := pool.QueryRow(ctx, `INSERT INTO users(login,password_hash,role) VALUES($1,$2,'admin') RETURNING id::text`, adminLogin, hash).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE login IN ($1,$2)`, adminLogin, userLogin)
		_, _ = pool.Exec(context.Background(), `UPDATE settings SET registration_enabled=false`)
	}()
	h := New(Dependencies{DB: pool, Auth: &auth.Service{DB: pool, TTL: 24 * time.Hour}})
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	registerBody := fmt.Sprintf(`{"login":%q,"password":"user secure password"}`, userLogin)
	if r := request("POST", "/api/v1/auth/register", registerBody, ""); r.Code != 403 {
		t.Fatalf("closed registration status %d", r.Code)
	}
	r := request("POST", "/api/v1/auth/login", fmt.Sprintf(`{"login":%q,"password":"admin secure password"}`, adminLogin), "")
	if r.Code != 200 {
		t.Fatalf("admin login: %d %s", r.Code, r.Body.String())
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(bytes.NewReader(r.Body.Bytes())).Decode(&login); err != nil || login.Token == "" {
		t.Fatalf("admin token: %v", err)
	}
	if r := request("PATCH", "/api/v1/admin/settings/registration", `{"enabled":true}`, login.Token); r.Code != 200 {
		t.Fatalf("enable registration: %d %s", r.Code, r.Body.String())
	}
	if r := request("POST", "/api/v1/auth/register", registerBody, ""); r.Code != 201 {
		t.Fatalf("registration: %d %s", r.Code, r.Body.String())
	}
	r = request("POST", "/api/v1/auth/login", fmt.Sprintf(`{"login":%q,"password":"user secure password"}`, userLogin), "")
	if r.Code != 200 {
		t.Fatalf("user login: %d %s", r.Code, r.Body.String())
	}
	var userSession struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &userSession)
	if rr := request("GET", "/api/v1/auth/me", "", userSession.Token); rr.Code != 200 {
		t.Fatalf("me: %d", rr.Code)
	}
	if rr := request("PATCH", "/api/v1/admin/settings/registration", `{"enabled":false}`, userSession.Token); rr.Code != 403 {
		t.Fatalf("user admin access: %d", rr.Code)
	}
	var userID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM users WHERE login=$1`, userLogin).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if rr := request("PATCH", "/api/v1/admin/users/"+userID, `{"disabled":true}`, login.Token); rr.Code != 200 {
		t.Fatalf("block: %d %s", rr.Code, rr.Body.String())
	}
	if rr := request("GET", "/api/v1/auth/me", "", userSession.Token); rr.Code != 401 {
		t.Fatalf("blocked session: %d", rr.Code)
	}
	if rr := request("PATCH", "/api/v1/admin/users/"+adminID, `{"disabled":true}`, login.Token); rr.Code != 409 {
		t.Fatalf("last admin blocked: %d", rr.Code)
	}
}
