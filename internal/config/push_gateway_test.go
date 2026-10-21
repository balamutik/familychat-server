package config

import (
	"strings"
	"testing"
)

func TestGatewayConfigurationRequiresHTTPSAndExclusiveCredentials(t *testing.T) {
	for _, tc := range []struct{ url, file, apns string }{
		{"http://push.example.com", "/token", ""}, {"https://user:pass@push.example.com", "/token", ""}, {"https://push.example.com/path", "/token", ""}, {"https://push.example.com?secret=x", "/token", ""}, {"https://push.example.com:0", "/token", ""}, {"https://push.example.com", "", ""}, {"", "/token", ""}, {"https://push.example.com", "/token", "/apple-key"},
	} {
		t.Run(tc.url+tc.file+tc.apns, func(t *testing.T) {
			t.Setenv("PUSH_GATEWAY_URL", tc.url)
			t.Setenv("PUSH_GATEWAY_TOKEN_FILE", tc.file)
			t.Setenv("APNS_KEY_FILE", tc.apns)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "push gateway") {
				t.Fatalf("wanted gateway validation, got %v", err)
			}
		})
	}
}
