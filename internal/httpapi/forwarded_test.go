package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageViewAlwaysIncludesForwardedFlag(t *testing.T) {
	for _, forwarded := range []bool{false, true} {
		body, err := json.Marshal(messageView{Forwarded: forwarded})
		if err != nil {
			t.Fatal(err)
		}
		want := `"forwarded":false`
		if forwarded {
			want = `"forwarded":true`
		}
		if !strings.Contains(string(body), want) {
			t.Fatalf("message response %s lacks %s", body, want)
		}
	}
}
