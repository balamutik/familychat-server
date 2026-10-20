package keyservice

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"familychat/server/internal/contentcrypto"
)

// Both paths exercise the independently built keyserver binary over real HTTPS.
func TestIndependentKeyServerHTTPSInterop(t *testing.T) {
	path, caPath := os.Getenv("KEYSERVER_TEST_CONFIG"), os.Getenv("KEYSERVER_TEST_CA")
	if path == "" || caPath == "" {
		t.Skip("set KEYSERVER_TEST_CONFIG and KEYSERVER_TEST_CA for independent HTTPS interop")
	}
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("bad test CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	config = config.WithHTTPClient(client)
	if len(config.PublicKey().Private) != 0 {
		t.Fatal("main API holds content private material")
	}
	fetch := func(method string, body []byte, authorization string) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, config.URL, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		if authorization != "" {
			request.Header.Set("Authorization", "Bearer "+authorization)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	proof := func(grant DeviceGrant, device *ecdsa.PrivateKey) []byte {
		t.Helper()
		message := "familychat-device-grant-v1\n" + config.KeyID + "\n" + grant.UserID + "\n" + grant.DeviceID + "\n" + grant.GrantID
		digest := sha256.Sum256([]byte(message))
		signature, err := ecdsa.SignASN1(rand.Reader, device, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(map[string]any{"version": 1, "user_id": grant.UserID, "device_id": grant.DeviceID, "grant_id": grant.GrantID, "signature": base64.StdEncoding.EncodeToString(signature)})
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	userID := "00000000-0000-4000-8000-000000000001"
	for _, deviceID := range []string{"00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003"} {
		device, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		public := base64.StdEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), device.X, device.Y))
		grant, err := config.Grant(t.Context(), userID, deviceID, public, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		// A wrong device cannot consume the real device's authorization.
		attacker, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		rejected := fetch(http.MethodPost, proof(grant, attacker), "")
		rejected.Body.Close()
		if rejected.StatusCode == 200 {
			t.Fatal("accepted proof from another device")
		}
		payload := proof(grant, device)
		response := fetch(http.MethodPost, payload, "")
		if response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" {
			response.Body.Close()
			t.Fatalf("external response %d", response.StatusCode)
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, 16385))
		response.Body.Close()
		if err != nil || len(raw) > 16384 {
			t.Fatal("invalid key response", err)
		}
		var key contentcrypto.Key
		if err = json.Unmarshal(raw, &key); err != nil || key.Validate() != nil || key.ID != config.KeyID {
			t.Fatal("external key does not match public API material", err)
		}
		text := "Прямое получение ключа клиентом 🔑"
		ciphertext, err := config.PublicKey().EncryptText(text, "interop")
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := base64.StdEncoding.DecodeString(ciphertext)
		if err != nil {
			t.Fatal(err)
		}
		var opened bytes.Buffer
		if err = key.Decrypt(&opened, bytes.NewReader(sealed), "interop"); err != nil || opened.String() != text {
			t.Fatal("key could not decrypt content", err)
		}
		replay := fetch(http.MethodPost, payload, "")
		replay.Body.Close()
		if replay.StatusCode == 200 {
			t.Fatal("consumed grant replay succeeded")
		}
		renewed, err := config.Grant(t.Context(), userID, deviceID, public, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		recovered := fetch(http.MethodPost, proof(renewed, device), "")
		recovered.Body.Close()
		if recovered.StatusCode != 200 {
			t.Fatal("fresh grant did not recover delivery", recovered.StatusCode)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rejected := fetch(method, nil, config.Secret)
		rejected.Body.Close()
		if rejected.StatusCode != 405 {
			t.Fatal("key delivery must require POST", rejected.StatusCode)
		}
	}
	registrationURL, err := url.Parse(config.URL)
	if err != nil {
		t.Fatal(err)
	}
	registrationURL.Path = "/api/v1/device-grants"
	registrationURL.RawPath = ""
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		request, err := http.NewRequestWithContext(t.Context(), method, registrationURL.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+config.Secret)
		request.Header.Set("X-FamilyChat-Issuer", config.Issuer)
		rejected, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		rejected.Body.Close()
		if rejected.StatusCode != 405 {
			t.Fatal("grant registration must require POST", rejected.StatusCode)
		}
	}
	for _, authorization := range []string{"primary-session-token", config.Secret} {
		rejected := fetch(http.MethodPost, []byte(`{}`), authorization)
		rejected.Body.Close()
		if rejected.StatusCode == 200 {
			t.Fatal("accepted bearer token without device proof")
		}
	}
}
