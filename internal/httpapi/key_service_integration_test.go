package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/contentcrypto"
	"familychat/server/internal/keyservice"
)

func TestExternalKeyDescriptorKeepsPrivateMaterialOffAPI(t *testing.T) {
	db := testDatabase(t)
	user, token := seedSession(t, db, "user")
	original, err := contentcrypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	service := &keyservice.Config{Scheme: "fc1", KeyID: original.ID, Public: base64.StdEncoding.EncodeToString(original.Public), URL: "https://keys.example.test:8443/api/v1/keys/current", Issuer: "fixture-family", Secret: base64.StdEncoding.EncodeToString(make([]byte, 32))}
	if err = service.Validate(); err != nil {
		t.Fatal(err)
	}
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
	if err = json.Unmarshal(response.Body.Bytes(), &delivered); err != nil {
		t.Fatal(err)
	}
	if delivered["key_id"] != original.ID || delivered["key_server_url"] != service.URL || delivered["ticket"] == "" {
		t.Fatal("invalid descriptor")
	}
	for _, name := range []string{"private_key", "ticket_secret", "public_key"} {
		if _, present := delivered[name]; present {
			t.Fatal("unexpected descriptor material", name)
		}
	}
	parts := strings.Split(delivered["ticket"], ".")
	if len(parts) != 3 {
		t.Fatal("invalid JWT")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err = json.Unmarshal(raw, &claims); err != nil || claims["sub"] != user || claims["iss"] != service.Issuer {
		t.Fatal("ticket not bound to authenticated account")
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
}
