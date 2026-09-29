package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"strconv"
	"time"

	"familychat/server/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type callView struct {
	ID        string    `json:"id"`
	ChatID    string    `json:"chat_id"`
	CallerID  string    `json:"caller_id"`
	CalleeID  string    `json:"callee_id"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

func registerCalls(mux *http.ServeMux, d Dependencies) {
	protected := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, require(d.Auth, fn)) }
	protected("POST /api/v1/chats/{id}/calls", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Kind string `json:"kind"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Kind != "audio" && body.Kind != "video" {
			writeError(w, 400, "invalid_kind")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		chatID := r.PathValue("id")
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		kind, _, err := chatRole(r, tx, chatID, p.UserID)
		if err != nil || kind != "direct" {
			writeError(w, 404, "not_found")
			return
		}
		var calleeID string
		err = tx.QueryRow(r.Context(), `SELECT cm.user_id::text FROM chat_members cm JOIN users u ON u.id=cm.user_id WHERE cm.chat_id=$1 AND cm.user_id<>$2 AND NOT u.disabled`, chatID, p.UserID).Scan(&calleeID)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		// Serialise starts for either participant, including calls in other chats.
		rows, err := tx.Query(r.Context(), `SELECT id FROM users WHERE id IN ($1,$2) ORDER BY id FOR UPDATE`, p.UserID, calleeID)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		var busy bool
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM calls WHERE state IN ('ringing','accepted') AND (caller_id IN ($1,$2) OR callee_id IN ($1,$2)))`, p.UserID, calleeID).Scan(&busy)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		if busy {
			writeError(w, 409, "busy")
			return
		}
		var call callView
		call.ChatID, call.CallerID, call.CalleeID, call.Kind, call.State = chatID, p.UserID, calleeID, body.Kind, "ringing"
		err = tx.QueryRow(r.Context(), `INSERT INTO calls(chat_id,caller_id,callee_id,caller_session_id,kind,state) VALUES($1,$2,$3,$4,$5,'ringing') RETURNING id::text,created_at`, chatID, p.UserID, calleeID, p.SessionID, body.Kind).Scan(&call.ID, &call.CreatedAt)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		if _, err = tx.Exec(r.Context(), `INSERT INTO events(chat_id,kind,payload) VALUES($1,'call_ringing',jsonb_build_object('call_id',$2::text,'caller_id',$3::text,'callee_id',$4::text,'kind',$5::text))`, chatID, call.ID, p.UserID, calleeID, body.Kind); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 201, call)
	})
	protected("GET /api/v1/calls/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		call, err := getCall(r.Context(), d.DB, r.PathValue("id"), p.UserID)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		writeJSON(w, 200, call)
	})
	for _, action := range []string{"accept", "reject", "cancel", "end"} {
		protected("POST /api/v1/calls/{id}/"+action, func(w http.ResponseWriter, r *http.Request) { changeCall(w, r, d, action) })
	}
	protected("GET /api/v1/calls/{id}/ice", func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		call, err := getCall(r.Context(), d.DB, r.PathValue("id"), p.UserID)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		if call.State != "accepted" {
			writeError(w, 409, "call_inactive")
			return
		}
		var selected bool
		if err := d.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM calls WHERE id=$1 AND ((caller_id=$2 AND caller_session_id=$3) OR (callee_id=$2 AND accepted_session_id=$3)))`, call.ID, p.UserID, p.SessionID).Scan(&selected); err != nil || !selected {
			writeError(w, 404, "not_found")
			return
		}
		if d.TurnURL == "" || d.TurnSecret == "" {
			writeError(w, 503, "turn_unavailable")
			return
		}
		expires := time.Now().Add(10 * time.Minute).Unix()
		username := strconv.FormatInt(expires, 10) + ":" + p.UserID
		mac := hmac.New(sha1.New, []byte(d.TurnSecret))
		_, _ = mac.Write([]byte(username))
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"ice_servers": []map[string]any{{"urls": []string{d.TurnURL}, "username": username, "credential": base64.StdEncoding.EncodeToString(mac.Sum(nil))}}, "expires_at": expires})
	})
}

func getCall(ctx context.Context, db *pgxpool.Pool, id, userID string) (callView, error) {
	var c callView
	if !validUUID(id) {
		return c, pgx.ErrNoRows
	}
	err := db.QueryRow(ctx, `SELECT c.id::text,c.chat_id::text,c.caller_id::text,c.callee_id::text,c.kind,c.state,c.created_at
		FROM calls c JOIN chats ch ON ch.id=c.chat_id JOIN chat_members cm ON cm.chat_id=c.chat_id AND cm.user_id=$2
		WHERE c.id=$1 AND ch.deleted_at IS NULL AND $2 IN (c.caller_id,c.callee_id)`, id, userID).Scan(&c.ID, &c.ChatID, &c.CallerID, &c.CalleeID, &c.Kind, &c.State, &c.CreatedAt)
	return c, err
}

func changeCall(w http.ResponseWriter, r *http.Request, d Dependencies, action string) {
	p, _ := auth.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	if !validUUID(id) {
		writeError(w, 404, "not_found")
		return
	}
	tx, err := d.DB.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "internal")
		return
	}
	defer tx.Rollback(r.Context())
	var c callView
	var acceptedSession string
	err = tx.QueryRow(r.Context(), `SELECT id::text,chat_id::text,caller_id::text,callee_id::text,kind,state,created_at,COALESCE(accepted_session_id::text,'') FROM calls WHERE id=$1 FOR UPDATE`, id).Scan(&c.ID, &c.ChatID, &c.CallerID, &c.CalleeID, &c.Kind, &c.State, &c.CreatedAt, &acceptedSession)
	if err != nil {
		writeError(w, 404, "not_found")
		return
	}
	if p.UserID != c.CallerID && p.UserID != c.CalleeID {
		writeError(w, 404, "not_found")
		return
	}
	if _, _, err = chatRole(r, tx, c.ChatID, p.UserID); err != nil {
		writeError(w, 404, "not_found")
		return
	}
	if (action == "accept" && p.UserID == c.CalleeID && c.State == "accepted" && acceptedSession == p.SessionID) ||
		(action == "reject" && p.UserID == c.CalleeID && c.State == "rejected") ||
		(action == "cancel" && p.UserID == c.CallerID && c.State == "cancelled") ||
		(action == "end" && c.State == "ended") {
		writeJSON(w, 200, c)
		return
	}
	newState := ""
	switch action {
	case "accept":
		if p.UserID == c.CalleeID && c.State == "ringing" && time.Since(c.CreatedAt) < 45*time.Second {
			newState = "accepted"
		}
	case "reject":
		if p.UserID == c.CalleeID && c.State == "ringing" {
			newState = "rejected"
		}
	case "cancel":
		if p.UserID == c.CallerID && c.State == "ringing" {
			newState = "cancelled"
		}
	case "end":
		if c.State == "accepted" {
			newState = "ended"
		}
	}
	if newState == "" {
		writeError(w, 409, "invalid_call_state")
		return
	}
	if newState == "accepted" {
		_, err = tx.Exec(r.Context(), `UPDATE calls SET state='accepted',accepted_session_id=$2,accepted_at=now() WHERE id=$1`, id, p.SessionID)
	} else {
		_, err = tx.Exec(r.Context(), `UPDATE calls SET state=$2,ended_at=now() WHERE id=$1`, id, newState)
	}
	if err != nil {
		writeError(w, 500, "internal")
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO events(chat_id,kind,payload) VALUES($1,$2,jsonb_build_object('call_id',$3::text,'state',$4::text))`, c.ChatID, "call_"+newState, id, newState); err != nil {
		writeError(w, 500, "internal")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "internal")
		return
	}
	c.State = newState
	writeJSON(w, 200, c)
}

func SweepCalls(ctx context.Context, db *pgxpool.Pool) error {
	rows, err := db.Query(ctx, `WITH expired AS (
		UPDATE calls SET state=CASE WHEN state='ringing' THEN 'missed' ELSE 'ended' END,ended_at=now()
		WHERE (state='ringing' AND created_at<now()-interval '45 seconds') OR
		(state='accepted' AND (caller_disconnected_at<now()-interval '30 seconds' OR callee_disconnected_at<now()-interval '30 seconds'))
		RETURNING id,chat_id)
		INSERT INTO events(chat_id,kind,payload) SELECT chat_id,'call_timeout',jsonb_build_object('call_id',id::text) FROM expired RETURNING id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}
