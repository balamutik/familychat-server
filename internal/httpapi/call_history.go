package httpapi

import (
	"familychat/server/internal/auth"
	"net/http"
	"time"
)

type callHistoryItem struct {
	ID            string     `json:"id"`
	ChatID        string     `json:"chat_id"`
	PeerID        string     `json:"peer_id"`
	PeerName      string     `json:"peer_name"`
	PeerAvatarURL *string    `json:"peer_avatar_url"`
	Kind          string     `json:"kind"`
	Direction     string     `json:"direction"`
	Outcome       string     `json:"outcome"`
	CreatedAt     time.Time  `json:"created_at"`
	ConnectedAt   *time.Time `json:"connected_at"`
	EndedAt       *time.Time `json:"ended_at"`
	CanRedial     bool       `json:"can_redial"`
}

func registerCallHistory(mux *http.ServeMux, d Dependencies) {
	mux.Handle("GET /api/v1/calls", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		before := r.URL.Query().Get("before")
		if before != "" && !validUUID(before) {
			writeError(w, 400, "invalid_cursor")
			return
		}
		var cursor any
		if before != "" {
			cursor = before
		}
		rows, err := d.DB.Query(r.Context(), `SELECT c.id::text,c.chat_id::text,u.id::text,u.login,CASE WHEN u.avatar_key IS NOT NULL AND NOT u.disabled THEN '/api/v1/users/' || u.id::text || '/avatar' ELSE NULL END,c.kind,
   CASE WHEN c.caller_id=$1 THEN 'outgoing' ELSE 'incoming' END,
   CASE WHEN c.state IN ('missed','rejected','cancelled') THEN c.state
        WHEN c.connected_at IS NOT NULL THEN 'completed'
        WHEN c.media_tracking THEN 'failed' ELSE 'unknown' END,
   c.created_at,c.connected_at,c.ended_at,NOT u.disabled
   FROM calls c JOIN chats ch ON ch.id=c.chat_id
   JOIN chat_members cm ON cm.chat_id=c.chat_id AND cm.user_id=$1
   JOIN users u ON u.id=CASE WHEN c.caller_id=$1 THEN c.callee_id ELSE c.caller_id END
   WHERE $1 IN (c.caller_id,c.callee_id) AND ch.deleted_at IS NULL
   AND c.state NOT IN ('ringing','accepted')
   AND ($2::uuid IS NULL OR (c.created_at,c.id)<(SELECT created_at,id FROM calls WHERE id=$2 AND $1 IN (caller_id,callee_id)))
   ORDER BY c.created_at DESC,c.id DESC LIMIT 51`, p.UserID, cursor)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer rows.Close()
		items := make([]callHistoryItem, 0)
		for rows.Next() {
			var item callHistoryItem
			if err := rows.Scan(&item.ID, &item.ChatID, &item.PeerID, &item.PeerName, &item.PeerAvatarURL, &item.Kind, &item.Direction, &item.Outcome, &item.CreatedAt, &item.ConnectedAt, &item.EndedAt, &item.CanRedial); err != nil {
				writeError(w, 500, "internal")
				return
			}
			items = append(items, item)
		}
		if rows.Err() != nil {
			writeError(w, 500, "internal")
			return
		}
		var next *string
		if len(items) > 50 {
			items = items[:50]
			last := items[49].ID
			next = &last
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
	})))
	mux.Handle("POST /api/v1/calls/{id}/connected", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		id := r.PathValue("id")
		call, err := getCall(r.Context(), d.DB, id, p.UserID)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		if call.State != "accepted" && call.State != "ended" {
			writeError(w, 409, "call_inactive")
			return
		}
		// Only the devices participating in this call can report the media connection.
		result, err := d.DB.Exec(r.Context(), `UPDATE calls SET connected_at=COALESCE(connected_at,LEAST(now(),COALESCE(ended_at,now()))),media_tracking=true
   WHERE id=$1 AND state IN ('accepted','ended') AND
   ((caller_id=$2 AND caller_session_id=$3) OR (callee_id=$2 AND accepted_session_id=$3))`, id, p.UserID, p.SessionID)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		if result.RowsAffected() == 0 {
			writeError(w, 404, "not_found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
}
