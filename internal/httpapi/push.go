package httpapi

import (
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"familychat/server/internal/auth"
)

var deviceTokenPattern = regexp.MustCompile(`^[a-fA-F0-9]{64,256}$`)

type pushDeviceRequest struct {
	ClientServerID string `json:"client_server_id"`
	Platform       string `json:"platform"`
	FCMToken       string `json:"fcm_token"`
	AlertToken     string `json:"alert_token"`
	VoIPToken      string `json:"voip_token"`
}

func validPushTokens(body pushDeviceRequest) bool {
	if body.Platform == "android" {
		return len(body.FCMToken) > 0 && len(body.FCMToken) <= 4096 && strings.IndexFunc(body.FCMToken, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) < 0 && body.AlertToken == "" && body.VoIPToken == ""
	}
	return (body.Platform == "" || body.Platform == "ios") && body.FCMToken == "" && (body.AlertToken != "" || body.VoIPToken != "") && (body.AlertToken == "" || deviceTokenPattern.MatchString(body.AlertToken)) && (body.VoIPToken == "" || deviceTokenPattern.MatchString(body.VoIPToken))
}
func registerPushDevices(mux *http.ServeMux, d Dependencies) {
	mux.Handle("PUT /api/v1/push/devices/{id}", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body pushDeviceRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		if !validUUID(id) || !validUUID(body.ClientServerID) || !validPushTokens(body) {
			writeError(w, 400, "invalid_push_device")
			return
		}
		if body.Platform == "" {
			body.Platform = "ios"
		}

		p, _ := auth.PrincipalFrom(r.Context())
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		// A token/session rotation invalidates queued jobs for the previous registration.
		_, err = tx.Exec(r.Context(), `DELETE FROM push_jobs j USING push_devices d WHERE j.device_id=d.id AND d.user_id=$1 AND d.device_id=$2 AND (d.session_id<>$3 OR d.client_server_id<>$4 OR d.platform<>$5 OR d.alert_token IS DISTINCT FROM NULLIF(lower($6),'') OR d.voip_token IS DISTINCT FROM NULLIF(lower($7),'') OR d.fcm_token IS DISTINCT FROM NULLIF($8,''))`, p.UserID, id, p.SessionID, body.ClientServerID, body.Platform, body.AlertToken, body.VoIPToken, body.FCMToken)
		if err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO push_devices(session_id,user_id,client_server_id,device_id,alert_token,voip_token,platform,fcm_token)
            VALUES($1,$2,$3,$4,NULLIF(lower($5),''),NULLIF(lower($6),''),$7,NULLIF($8,''))
            ON CONFLICT(user_id,device_id) DO UPDATE SET session_id=EXCLUDED.session_id,client_server_id=EXCLUDED.client_server_id,
            alert_token=EXCLUDED.alert_token,voip_token=EXCLUDED.voip_token,platform=EXCLUDED.platform,fcm_token=EXCLUDED.fcm_token,updated_at=now()`,
				p.SessionID, p.UserID, body.ClientServerID, id, body.AlertToken, body.VoIPToken, body.Platform, body.FCMToken)
		}
		if err == nil {
			err = tx.Commit(r.Context())
		}
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
