package httpapi

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/contentcrypto"
	"familychat/server/internal/keyservice"
)

func externalKeyService(t *testing.T, endpoint string) (*keyservice.Config, *contentcrypto.Key) {
	t.Helper()
	original, err := contentcrypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	service := &keyservice.Config{Scheme: "fc1", KeyID: original.ID, Public: base64.StdEncoding.EncodeToString(original.Public), URL: endpoint + "/api/v1/keys/current", Issuer: "fixture-family", Secret: base64.StdEncoding.EncodeToString(make([]byte, 32))}
	if err = service.Validate(); err != nil {
		t.Fatal(err)
	}
	return service, original
}

func devicePublicKey(t *testing.T) string {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key.PublicKey().Bytes())
}

func TestExternalKeyDescriptorKeepsPrivateMaterialOffAPI(t *testing.T) {
	db := testDatabase(t)
	user, token := seedSession(t, db, "user")
	service, original := externalKeyService(t, "https://keys.example.test:8443")
	public := service.PublicKey()
	if len(public.Private) != 0 {
		t.Fatal("main API received private content material")
	}
	handler := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, ContentKey: public, KeyService: service})
	if response := callAPI(handler, "GET", "/api/v1/encryption/key", "", ""); response.Code != 401 {
		t.Fatal("descriptor available without authentication")
	}
	response := callAPI(handler, "GET", "/api/v1/encryption/key", "", token)
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("descriptor status %d", response.Code)
	}
	var delivered map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &delivered); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 4 || delivered["scheme"] != "fc1" || delivered["key_id"] != original.ID || delivered["key_server_url"] != service.URL || delivered["key_delivery"] != "device-grant-v1" {
		t.Fatal("invalid descriptor", response.Body.String())
	}
	// A public-only API remains able to validate the fc1 ciphertext envelope.
	ciphertext, err := original.EncryptText("Семейная переписка", contentcrypto.MessageContext("chat", user, "message"))
	if err != nil || public.ValidateText(ciphertext) != nil {
		t.Fatal("public-only validation failed", err)
	}
	handler = New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, ContentKey: public})
	if unavailable := callAPI(handler, "GET", "/api/v1/encryption/key", "", token); unavailable.Code != 503 {
		t.Fatal("public-only config silently exposed empty private keys")
	}
	// Existing local-key mode preserves its legacy response.
	handler = New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, ContentKey: original})
	response = callAPI(handler, "GET", "/api/v1/encryption/key", "", token)
	var legacy contentcrypto.Key
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &legacy) != nil || legacy.Validate() != nil || legacy.ID != original.ID {
		t.Fatal("legacy local key delivery failed")
	}
}

func TestDeviceGrantsBindAuthenticatedUserAndAllowIndependentDevices(t *testing.T) {
	db := testDatabase(t)
	user, token := seedSession(t, db, "user")
	registrations := make(chan keyservice.DeviceGrant, 4)
	var secret string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/device-grants" {
			t.Errorf("unexpected upstream request %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("Authorization") == "Bearer "+token || r.Header.Get("X-FamilyChat-Issuer") != "fixture-family" {
			t.Error("server authentication mismatch or session leaked")
		}
		var grant keyservice.DeviceGrant
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&grant); err != nil {
			t.Error(err)
		}
		registrations <- grant
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	service, _ := externalKeyService(t, server.URL)
	secret = service.Secret
	service = service.WithHTTPClient(server.Client())
	handler := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, ContentKey: service.PublicKey(), KeyService: service})
	previous := ""
	for _, id := range []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001"} {
		before := time.Now().Unix()
		response := callAPI(handler, "POST", "/api/v1/encryption/device-grant", fmt.Sprintf(`{"device_id":%q,"device_public_key":%q}`, id, devicePublicKey(t)), token)
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("grant response %d %s", response.Code, response.Body.String())
		}
		var result struct {
			Scheme    string `json:"scheme"`
			KeyID     string `json:"key_id"`
			URL       string `json:"key_server_url"`
			Delivery  string `json:"key_delivery"`
			UserID    string `json:"user_id"`
			DeviceID  string `json:"device_id"`
			GrantID   string `json:"grant_id"`
			ExpiresAt int64  `json:"expires_at"`
		}
		decoder := json.NewDecoder(response.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&result); err != nil {
			t.Fatal("unexpected response material", err)
		}
		registered := <-registrations
		if result.Scheme != "fc1" || result.KeyID != service.KeyID || result.URL != service.URL || result.Delivery != "device-grant-v1" || result.UserID != user || result.DeviceID != id || registered.UserID != user || registered.DeviceID != id || registered.GrantID != result.GrantID || registered.ExpiresAt != result.ExpiresAt || registered.Version != 1 || registered.KeyID != service.KeyID || result.ExpiresAt < before+120 || result.ExpiresAt > time.Now().Unix()+120 {
			t.Fatalf("invalid registration/response %+v %+v", result, registered)
		}
		decoded, err := hex.DecodeString(result.GrantID)
		if err != nil || len(decoded) != 32 || strings.ToLower(result.GrantID) != result.GrantID || result.GrantID == previous {
			t.Fatal("invalid or reused grant")
		}
		previous = result.GrantID
	}
}

func TestDeviceGrantRejectsUntrustedBodyAndUnauthenticatedRequests(t *testing.T) {
	db := testDatabase(t)
	_, token := seedSession(t, db, "user")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(204) }))
	defer server.Close()
	service, _ := externalKeyService(t, server.URL)
	handler := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, ContentKey: service.PublicKey(), KeyService: service})
	id := "00000000-0000-4000-8000-000000000001"
	public := devicePublicKey(t)
	valid := fmt.Sprintf(`{"device_id":%q,"device_public_key":%q}`, id, public)
	if response := callAPI(handler, "POST", "/api/v1/encryption/device-grant", valid, ""); response.Code != 401 {
		t.Fatal("anonymous registration accepted")
	}
	if response := callAPI(handler, "GET", "/api/v1/encryption/device-grant", "", token); response.Code != 405 {
		t.Fatal("grant registration must require POST", response.Code)
	}
	for _, body := range []string{
		`{}`, `null`, valid + `{}`, valid + ` true`,
		fmt.Sprintf(`{"device_id":%q,"device_public_key":%q,"user_id":%q}`, id, public, id),
		fmt.Sprintf(`{"device_id":%q,"device_public_key":%q,"ticket":"forged"}`, id, public),
		fmt.Sprintf(`{"device_id":"00000000-0000-4000-8000-00000000000A","device_public_key":%q}`, public),
		fmt.Sprintf(`{"device_id":%q,"device_public_key":"bad"}`, id),
		fmt.Sprintf(`{"device_id":%q,"device_public_key":%q}`, id, public+"\n"),
		strings.Repeat(" ", 8193) + valid,
	} {
		if response := callAPI(handler, "POST", "/api/v1/encryption/device-grant", body, token); response.Code != 400 {
			t.Errorf("accepted malformed or spoofed body: %d %s", response.Code, body)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("untrusted requests reached key server")
	}
}

func TestDeviceGrantWaitsForRegistrationAndFailsClosed(t *testing.T) {
	db := testDatabase(t)
	_, token := seedSession(t, db, "user")
	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(500) }))
	defer server.Close()
	defer func() {
		if !released {
			close(release)
		}
	}()
	service, _ := externalKeyService(t, server.URL)
	handler := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, ContentKey: service.PublicKey(), KeyService: service})
	done := make(chan *httptest.ResponseRecorder, 1)
	body := fmt.Sprintf(`{"device_id":"00000000-0000-4000-8000-000000000001","device_public_key":%q}`, devicePublicKey(t))
	go func() { done <- callAPI(handler, "POST", "/api/v1/encryption/device-grant", body, token) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("registration did not reach keyserver")
	}
	select {
	case <-done:
		t.Fatal("grant returned before keyserver acknowledgement")
	default:
	}
	close(release)
	released = true
	response := <-done
	if response.Code != 503 || response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "grant_id") || strings.Contains(response.Body.String(), "private_key") {
		t.Fatalf("upstream failure did not fail closed %d %s", response.Code, response.Body.String())
	}
	// An external service failure cannot fall back to the legacy private key.
	local, _ := contentcrypto.Generate()
	handler = New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, ContentKey: local})
	response = callAPI(handler, "POST", "/api/v1/encryption/device-grant", body, token)
	if response.Code != 503 || strings.Contains(response.Body.String(), "private_key") {
		t.Fatal("grant endpoint fell back to local keys")
	}
}
