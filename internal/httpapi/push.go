package httpapi

import (
	"net/http"
	"regexp"

	"familychat/server/internal/auth"
)

var deviceTokenPattern = regexp.MustCompile(`^[a-fA-F0-9]{64,256}$`)

func registerPushDevices(mux *http.ServeMux, d Dependencies) {
	mux.Handle("PUT /api/v1/push/devices/{id}", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body struct {
			ClientServerID string `json:"client_server_id"`
			AlertToken     string `json:"alert_token"`
			VoIPToken      string `json:"voip_token"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if !validUUID(id) || !validUUID(body.ClientServerID) ||
			(body.AlertToken == "" && body.VoIPToken == "") ||
			(body.AlertToken != "" && !deviceTokenPattern.MatchString(body.AlertToken)) ||
			(body.VoIPToken != "" && !deviceTokenPattern.MatchString(body.VoIPToken)) {
			writeError(w, 400, "invalid_push_device")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		_, err := d.DB.Exec(r.Context(), `INSERT INTO push_devices(session_id,user_id,client_server_id,device_id,alert_token,voip_token)
            VALUES($1,$2,$3,$4,NULLIF(lower($5),''),NULLIF(lower($6),''))
            ON CONFLICT(user_id,device_id) DO UPDATE SET session_id=EXCLUDED.session_id,client_server_id=EXCLUDED.client_server_id,
            alert_token=EXCLUDED.alert_token,voip_token=EXCLUDED.voip_token,updated_at=now()`,
			p.SessionID, p.UserID, body.ClientServerID, id, body.AlertToken, body.VoIPToken)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	mux.Handle("DELETE /api/v1/push/devices/{id}", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validUUID(id) {
			writeError(w, 404, "not_found")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		if _, err := d.DB.Exec(r.Context(), `DELETE FROM push_devices WHERE session_id=$1 AND device_id=$2`, p.SessionID, id); err != nil {
			writeError(w, 500, "internal")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
}
