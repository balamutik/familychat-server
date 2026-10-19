package httpapi

import (
	"net/http"
	"time"

	"familychat/server/internal/auth"
)

func registerContentEncryption(mux *http.ServeMux, d Dependencies) {
	mux.Handle("GET /api/v1/encryption/key", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		if d.ContentKey == nil {
			writeError(w, 503, "encryption_unavailable")
			return
		}
		if d.KeyService != nil {
			principal, ok := auth.PrincipalFrom(r.Context())
			if !ok {
				writeError(w, 401, "unauthorized")
				return
			}
			ticket, expiry, err := d.KeyService.Ticket(principal.UserID, time.Now())
			if err != nil {
				writeError(w, 503, "encryption_unavailable")
				return
			}
			writeJSON(w, 200, struct {
				Scheme  string `json:"scheme"`
				ID      string `json:"key_id"`
				URL     string `json:"key_server_url"`
				Ticket  string `json:"ticket"`
				Expires string `json:"ticket_expires_at"`
			}{"fc1", d.ContentKey.ID, d.KeyService.URL, ticket, expiry.UTC().Format(time.RFC3339)})
			return
		}
		if len(d.ContentKey.Private) != 32 {
			writeError(w, 503, "encryption_unavailable")
			return
		}
		writeJSON(w, 200, struct {
			Scheme  string `json:"scheme"`
			ID      string `json:"key_id"`
			Public  []byte `json:"public_key"`
			Private []byte `json:"private_key"`
		}{"fc1", d.ContentKey.ID, d.ContentKey.Public, d.ContentKey.Private})
	})))
}
