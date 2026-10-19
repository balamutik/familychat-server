package keyservice

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"familychat/server/internal/contentcrypto"
)

// Set both paths to exercise the independently built keyserver binary over real
// HTTPS, rather than replacing either side of the exchange with a stub.
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
	fetch := func(ticket string) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, config.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+ticket)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	ticket, _, err := config.Ticket("00000000-0000-4000-8000-000000000001", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	response := fetch(ticket)
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("external response %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16385))
	if err != nil || len(raw) > 16384 {
		t.Fatal("invalid key response", err)
	}
	var key contentcrypto.Key
	if err = json.Unmarshal(raw, &key); err != nil || key.Validate() != nil || key.ID != config.KeyID {
		t.Fatal("external key does not match public API material", err)
	}
	if len(config.PublicKey().Private) != 0 {
		t.Fatal("main API holds content private material")
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
	for _, invalid := range []string{"primary-session-token", ticket + "bad"} {
		rejected := fetch(invalid)
		rejected.Body.Close()
		if rejected.StatusCode == 200 {
			t.Fatal("accepted non-ticket authorization")
		}
	}
	expired, _, _ := config.Ticket("00000000-0000-4000-8000-000000000001", time.Now().Add(-10*time.Minute))
	rejected := fetch(expired)
	rejected.Body.Close()
	if rejected.StatusCode == 200 {
		t.Fatal("accepted expired main-server ticket")
	}
}
