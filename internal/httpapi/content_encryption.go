package httpapi

import "net/http"

func registerContentEncryption(mux *http.ServeMux, d Dependencies) {
	mux.Handle("GET /api/v1/encryption/key", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		if d.ContentKey == nil {
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
