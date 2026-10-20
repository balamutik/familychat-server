package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/keyservice"
)

type keyServiceDescriptor struct {
	Scheme   string `json:"scheme"`
	ID       string `json:"key_id"`
	URL      string `json:"key_server_url"`
	Delivery string `json:"key_delivery"`
}

func registerContentEncryption(mux *http.ServeMux, d Dependencies) {
	mux.Handle("GET /api/v1/encryption/key", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		if d.ContentKey == nil {
			writeError(w, 503, "encryption_unavailable")
			return
		}
		if d.KeyService != nil {
			writeJSON(w, 200, keyServiceDescriptor{"fc1", d.KeyService.KeyID, d.KeyService.URL, "device-grant-v1"})
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
	mux.Handle("POST /api/v1/encryption/device-grant", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		if d.KeyService == nil || d.ContentKey == nil {
			writeError(w, 503, "encryption_unavailable")
			return
		}
		principal, ok := auth.PrincipalFrom(r.Context())
		if !ok {
			writeError(w, 401, "unauthorized")
			return
		}
		var body struct {
			DeviceID        string `json:"device_id"`
			DevicePublicKey string `json:"device_public_key"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		defer r.Body.Close()
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeError(w, 400, "invalid_json")
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			writeError(w, 400, "invalid_json")
			return
		}
		if err := keyservice.ValidateDevice(body.DeviceID, body.DevicePublicKey); err != nil {
			writeError(w, 400, "invalid_device")
			return
		}
		grant, err := d.KeyService.Grant(r.Context(), principal.UserID, body.DeviceID, body.DevicePublicKey, time.Now())
		if err != nil {
			writeError(w, 503, "encryption_unavailable")
			return
		}
		writeJSON(w, 200, struct {
			keyServiceDescriptor
			UserID    string `json:"user_id"`
			DeviceID  string `json:"device_id"`
			GrantID   string `json:"grant_id"`
			ExpiresAt int64  `json:"expires_at"`
		}{keyServiceDescriptor{"fc1", d.KeyService.KeyID, d.KeyService.URL, "device-grant-v1"}, grant.UserID, grant.DeviceID, grant.GrantID, grant.ExpiresAt})
	})))
}
