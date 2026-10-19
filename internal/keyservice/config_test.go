package keyservice

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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

func TestConfigContainsOnlyPublicContentKeyAndSignsBoundedTicket(t *testing.T) {
	config, _, key := fixture(t)
	if len(config.PublicKey().Private) != 0 || config.PublicKey().ID != key.ID {
		t.Fatal("API retained a content private key")
	}
	now := time.Unix(1_800_000_000, 0)
	token, expiry, err := config.Ticket("00000000-0000-4000-8000-000000000001", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("not compact JWT")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, make([]byte, 32))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(mac.Sum(nil), signature) {
		t.Fatal("invalid HS256 signature")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != "family-test" || claims["aud"] != "familychat-keyserver" || claims["key_id"] != key.ID || claims["exp"] != float64(expiry.Unix()) || expiry.Sub(now) != 120*time.Second {
		t.Fatal("wrong ticket claims")
	}
	if strings.Contains(string(payload), "private_key") || strings.Contains(string(payload), "ticket_secret") {
		t.Fatal("key material in ticket")
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
		t.Fatal("accepted world-readable ticket credentials")
	}
}
