package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONRejectsMalformedTrailingData(t *testing.T) {
	for _, body := range []string{`{"enabled":true} garbage`, `{"enabled":true} {`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		w := httptest.NewRecorder()
		var v struct {
			Enabled bool `json:"enabled"`
		}
		if decodeJSON(w, r, &v) || w.Code != 400 {
			t.Fatalf("accepted %q: %d", body, w.Code)
		}
	}
}
