package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"familychat/server/internal/auth"
	"github.com/coder/websocket"
)

func registerWebsocket(mux *http.ServeMux, d Dependencies) {
	mux.Handle("POST /api/v1/auth/websocket-ticket", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		ticket, err := d.Auth.CreateWebSocketTicket(r.Context(), p)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 201, map[string]string{"ticket": ticket})
	})))
	mux.HandleFunc("GET /api/v1/ws", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if d.Auth == nil || d.Events == nil {
			writeError(w, 503, "unavailable")
			return
		}
		p, err := d.Auth.ConsumeWebSocketTicket(r.Context(), r.URL.Query().Get("ticket"))
		if err != nil {
			writeError(w, 401, "unauthorized")
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: d.AllowedOrigins})
		if err != nil {
			return
		}
		conn.SetReadLimit(64 << 10)
		client := d.Events.Attach(p.UserID, p.SessionID, conn)
		recordUserSeen(r.Context(), d, p.UserID)
		pingDone := make(chan struct{})
		go func() {
			ticker := time.NewTicker(25 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-pingDone:
					return
				case <-ticker.C:
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					err := conn.Ping(ctx)
					cancel()
					if err != nil {
						client.Close()
						return
					}
					recordUserSeen(context.Background(), d, p.UserID)
				}
			}
		}()
		_, _ = d.DB.Exec(context.Background(), `UPDATE calls SET caller_disconnected_at=NULL WHERE caller_session_id=$1 AND state='accepted'`, p.SessionID)
		_, _ = d.DB.Exec(context.Background(), `UPDATE calls SET callee_disconnected_at=NULL WHERE accepted_session_id=$1 AND state='accepted'`, p.SessionID)
		defer func() {
			close(pingDone)
			client.Close()
			recordUserSeen(context.Background(), d, p.UserID)
			if d.Events.HasSession(p.SessionID) {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = d.DB.Exec(ctx, `UPDATE calls SET caller_disconnected_at=now() WHERE caller_session_id=$1 AND state='accepted' AND caller_disconnected_at IS NULL`, p.SessionID)
			_, _ = d.DB.Exec(ctx, `UPDATE calls SET callee_disconnected_at=now() WHERE accepted_session_id=$1 AND state='accepted' AND callee_disconnected_at IS NULL`, p.SessionID)
		}()
		window := time.Now()
		count := 0
		for {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var frame struct {
				Type         string          `json:"type"`
				AfterEventID int64           `json:"after_event_id"`
				CallID       string          `json:"call_id"`
				Data         json.RawMessage `json:"data"`
			}
			if json.Unmarshal(payload, &frame) != nil {
				client.Close()
				return
			}
			switch frame.Type {
			case "sync":
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = d.Events.Replay(ctx, client, frame.AfterEventID)
				cancel()
			case "offer", "answer", "ice":
				if time.Since(window) >= time.Second {
					window = time.Now()
					count = 0
				}
				count++
				if count > 30 || len(frame.Data) == 0 || len(frame.Data) > 32<<10 || !validUUID(frame.CallID) || !relaySignal(r.Context(), d, p, frame.Type, frame.CallID, frame.Data) {
					client.Close()
					return
				}
			default:
				client.Close()
				return
			}
		}
	})
}

func relaySignal(ctx context.Context, d Dependencies, p auth.Principal, kind, callID string, data json.RawMessage) bool {
	var target string
	err := d.DB.QueryRow(ctx, `SELECT CASE WHEN c.caller_session_id=$2 THEN c.accepted_session_id::text ELSE c.caller_session_id::text END
		FROM calls c JOIN chats ch ON ch.id=c.chat_id JOIN chat_members cm ON cm.chat_id=c.chat_id AND cm.user_id=$3
		JOIN sessions s ON s.id=$2 AND s.user_id=$3 AND s.revoked_at IS NULL AND s.expires_at>now()
		JOIN users u ON u.id=$3 AND NOT u.disabled
		WHERE c.id=$1 AND c.state='accepted' AND ch.deleted_at IS NULL AND
		((c.caller_id=$3 AND c.caller_session_id=$2) OR (c.callee_id=$3 AND c.accepted_session_id=$2)) AND
		EXISTS(SELECT 1 FROM sessions peer JOIN users pu ON pu.id=peer.user_id WHERE peer.id=CASE WHEN c.caller_session_id=$2 THEN c.accepted_session_id ELSE c.caller_session_id END AND peer.revoked_at IS NULL AND peer.expires_at>now() AND NOT pu.disabled AND peer.user_id=CASE WHEN c.caller_session_id=$2 THEN c.callee_id ELSE c.caller_id END)`, callID, p.SessionID, p.UserID).Scan(&target)
	if err != nil {
		return false
	}
	payload, err := json.Marshal(map[string]any{"type": kind, "call_id": callID, "data": data})
	if err != nil {
		return false
	}
	return d.Events.SendToSession(target, payload)
}

// Persist the last confirmed connection activity. Abrupt disconnects are
// detected by the existing WebSocket ping/pong deadline.
func recordUserSeen(parent context.Context, d Dependencies, userID string) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	_, _ = d.DB.Exec(ctx, `UPDATE users SET last_seen_at=GREATEST(last_seen_at,now()) WHERE id=$1`, userID)
}
