package keyservice

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"familychat/server/internal/contentcrypto"
)

func fixture(t *testing.T) (*Config, string, *contentcrypto.Key) {
	t.Helper()
	key, err := contentcrypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"scheme": "fc1", "key_id": key.ID, "public_key": base64.StdEncoding.EncodeToString(key.Public), "key_server_url": "https://keys.example.test:8443/api/v1/keys/current", "ticket_issuer": "family-test", "ticket_secret": base64.StdEncoding.EncodeToString(make([]byte, 32))})
	path := filepath.Join(t.TempDir(), "key-service.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return config, path, key
}

func TestGrantRegistersOnlyDeviceAuthorizationBeforeReturning(t *testing.T) {
	config, _, key := fixture(t)
	if len(config.PublicKey().Private) != 0 || config.PublicKey().ID != key.ID {
		t.Fatal("API retained a content private key")
	}
	device, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public := base64.StdEncoding.EncodeToString(device.PublicKey().Bytes())
	now := time.Unix(1_800_000_000, 0)
	var registered DeviceGrant
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/device-grants" || r.URL.RawQuery != "" {
			t.Errorf("wrong registration target: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer "+config.Secret || r.Header.Get("X-FamilyChat-Issuer") != "family-test" {
			t.Error("wrong server credentials")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong content type")
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&registered); err != nil {
			t.Error(err)
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	config.URL = server.URL + "/api/v1/keys/current"
	config = config.WithHTTPClient(server.Client())
	grant, err := config.Grant(t.Context(), "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", public, now)
	if err != nil {
		t.Fatal(err)
	}
	if grant != registered || registered.Version != 1 || registered.UserID != "00000000-0000-4000-8000-000000000001" || registered.DeviceID != "00000000-0000-4000-8000-000000000002" || registered.DevicePublicKey != public || registered.KeyID != key.ID || registered.ExpiresAt != 1_800_000_120 {
		t.Fatalf("wrong registered grant: %+v", registered)
	}
	decoded, err := hex.DecodeString(grant.GrantID)
	if err != nil || len(decoded) != 32 || strings.ToLower(grant.GrantID) != grant.GrantID {
		t.Fatal("invalid grant ID")
	}
	other, err := config.Grant(t.Context(), registered.UserID, registered.DeviceID, public, now)
	if err != nil || other.GrantID == grant.GrantID {
		t.Fatal("fresh grants must be independent", err)
	}
}

func TestGrantRejectsInvalidDevicesWithoutCallingKeyServer(t *testing.T) {
	config, _, _ := fixture(t)
	device, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public := base64.StdEncoding.EncodeToString(device.PublicKey().Bytes())
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer server.Close()
	config.URL = server.URL + "/api/v1/keys/current"
	for _, item := range []struct{ user, device, public string }{
		{"", "00000000-0000-4000-8000-000000000002", public},
		{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-00000000000A", public},
		{"00000000-0000-4000-8000-000000000001", "bad", public},
		{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "bad"},
		{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", public + "\n"},
		{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", base64.StdEncoding.EncodeToString(make([]byte, 65))},
	} {
		if _, err := config.Grant(t.Context(), item.user, item.device, item.public, time.Now()); err == nil {
			t.Error("accepted invalid device", item)
		}
	}
	if calls != 0 {
		t.Fatal("invalid grant reached keyserver")
	}
}

func TestGrantFailsClosedOnUpstreamResponsesAndRedirects(t *testing.T) {
	device, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public := base64.StdEncoding.EncodeToString(device.PublicKey().Bytes())
	for _, status := range []int{200, 201, 400, 401, 500, 301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			config, _, _ := fixture(t)
			followed := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				followed++
				t.Error("redirect leaked a request")
				w.WriteHeader(204)
			}))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				io.WriteString(w, strings.Repeat("x", 20000))
			}))
			defer server.Close()
			config.URL = server.URL + "/api/v1/keys/current"
			config = config.WithHTTPClient(server.Client())
			if grant, err := config.Grant(t.Context(), "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", public, time.Now()); err == nil || grant.GrantID != "" {
				t.Fatal("accepted unacknowledged grant")
			}
			if followed != 0 {
				t.Fatal("followed redirect")
			}
		})
	}
}

func TestGrantHonorsCancellation(t *testing.T) {
	config, _, _ := fixture(t)
	device, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("cancelled request reached server")
		w.WriteHeader(204)
	}))
	defer server.Close()
	config.URL = server.URL + "/api/v1/keys/current"
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if grant, err := config.Grant(ctx, "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", base64.StdEncoding.EncodeToString(device.PublicKey().Bytes()), time.Now()); err == nil || grant.GrantID != "" {
		t.Fatal("cancelled grant succeeded")
	}
}

func TestConfigRejectsUnsafeOrMismatchedIntegration(t *testing.T) {
	_, path, _ := fixture(t)
	original, _ := os.ReadFile(path)
	cases := []map[string]any{
		{"key_server_url": "http://keys.example.test:8443/api/v1/keys/current"},
		{"key_server_url": "https://user:password@keys.example.test:8443/api/v1/keys/current"},
		{"key_server_url": "https://keys.example.test:443/api/v1/keys/current"},
		{"key_id": strings.Repeat("0", 64)}, {"ticket_secret": "bad"}, {"private_key": "never-on-main"},
	}
	for _, changes := range cases {
		var item map[string]any
		json.Unmarshal(original, &item)
		for k, v := range changes {
			item[k] = v
		}
		raw, _ := json.Marshal(item)
		os.WriteFile(path, raw, 0600)
		if _, err := Load(path); err == nil {
			t.Errorf("accepted %v", changes)
		}
	}
	os.WriteFile(path, original, 0600)
	os.Chmod(path, 0644)
	if _, err := Load(path); err == nil {
		t.Fatal("accepted world-readable registration credentials")
	}
}
