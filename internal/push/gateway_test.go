package push

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"familychat/server/internal/config"
	"familychat/server/internal/pushgateway"
)

type gatewaySink struct {
	payload json.RawMessage
	kind    string
	count   int
	invalid bool
}

func (s *gatewaySink) Send(_ context.Context, _ string, kind string, payload json.RawMessage) (bool, error) {
	s.payload = payload
	s.kind = kind
	s.count++
	return s.invalid, nil
}

const gatewayEvent = `{"aps":{"alert":{"title":"Private sender","body":"private message"}},"server_id":"10000000-0000-0000-0000-000000000001","chat_id":"10000000-0000-0000-0000-000000000002","message_id":"10000000-0000-0000-0000-000000000003","sender_name":"private sender"}`

func TestGatewayEndToEndTLSAndPrivacy(t *testing.T) {
	store, err := pushgateway.Open(filepath.Join(t.TempDir(), "state", "gateway.json"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.AddClient("family")
	if err != nil {
		t.Fatal(err)
	}
	sink := &gatewaySink{}
	server := httptest.NewTLSServer(pushgateway.NewHandler(store, sink, true, true))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "token")
	if err = os.WriteFile(file, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(config.Config{PushGatewayURL: server.URL, PushGatewayTokenFile: file})
	if err != nil {
		t.Fatal(err)
	}
	router.Gateway.http.Transport = server.Client().Transport
	invalid, err := router.Send(context.Background(), strings.Repeat("a", 64), "alert", json.RawMessage(gatewayEvent))
	if err != nil || invalid || sink.count != 1 {
		t.Fatalf("delivery invalid=%v err=%v count=%d", invalid, err, sink.count)
	}
	if strings.Contains(string(sink.payload), "private") || strings.Contains(string(sink.payload), "Private") {
		t.Fatal("private data leaked")
	}
	sink.invalid = true
	invalid, err = router.Send(context.Background(), strings.Repeat("a", 64), "alert", json.RawMessage(gatewayEvent))
	if !invalid || err == nil {
		t.Fatal("permanent result not propagated")
	}
	if err = store.RevokeClient(strings.Split(token, ".")[0]); err != nil {
		t.Fatal(err)
	}
	invalid, err = router.Send(context.Background(), strings.Repeat("a", 64), "alert", json.RawMessage(gatewayEvent))
	if invalid || err == nil {
		t.Fatal("revocation must not delete a valid device")
	}
}
func TestGatewayRejectsRedirectsAndRedactsProviderErrors(t *testing.T) {
	token := "AAAAAAAAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte(token), 0600)
	for _, code := range []int{301, 307, 401, 429, 500, 410} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "https://secret.invalid/")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "secret response "+token)
			}))
			defer server.Close()
			g, err := NewGateway(config.Config{PushGatewayURL: server.URL, PushGatewayTokenFile: file})
			if err != nil {
				t.Fatal(err)
			}
			g.http.Transport = server.Client().Transport
			invalid, err := g.Send(context.Background(), strings.Repeat("a", 64), "alert", json.RawMessage(gatewayEvent))
			if invalid || err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe result: %v %v", invalid, err)
			}
			if calls != 1 {
				t.Fatal("redirect followed")
			}
		})
	}
}
func TestGatewayCredentialPermissions(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("AAAAAAAAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"), 0644)
	if _, err := NewGateway(config.Config{PushGatewayURL: "https://push.example.com", PushGatewayTokenFile: file}); err == nil {
		t.Fatal("world readable secret accepted")
	}
}
