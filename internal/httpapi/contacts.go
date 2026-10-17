package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"familychat/server/internal/auth"
	"github.com/jackc/pgx/v5"
)

type contactProfile struct {
	ID          string        `json:"id"`
	Login       string        `json:"login"`
	DisplayName string        `json:"display_name,omitempty"`
	AvatarURL   string        `json:"avatar_url,omitempty"`
	Presence    *peerPresence `json:"presence,omitempty"`
	BlockedByMe bool          `json:"blocked_by_me"`
	CanMessage  bool          `json:"can_message"`
	CanCall     bool          `json:"can_call"`
}

// Existing direct-chat mutations lock the chat first. Pair locking also covers
// blocking before a direct chat exists, racing its creation.
func lockContactPair(ctx context.Context, tx pgx.Tx, a, b string) error {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a > b {
		a, b = b, a
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,5514))`, a+":"+b)
	return err
}

func directCommunicationAllowed(ctx context.Context, tx pgx.Tx, chatID string) (bool, error) {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT kind<>'direct' OR (NOT EXISTS(SELECT 1 FROM users WHERE id IN (c.direct_user_low,c.direct_user_high) AND disabled)
 AND NOT EXISTS(SELECT 1 FROM user_blocks b WHERE (b.source_user_id=c.direct_user_low AND b.target_user_id=c.direct_user_high) OR (b.source_user_id=c.direct_user_high AND b.target_user_id=c.direct_user_low))) FROM chats c WHERE id=$1`, chatID).Scan(&allowed)
	return allowed, err
}
func requireDirectCommunication(w http.ResponseWriter, r *http.Request, tx pgx.Tx, chatID string) bool {
	allowed, err := directCommunicationAllowed(r.Context(), tx, chatID)
	if err != nil {
		writeError(w, 500, "internal")
		return false
	}
	if !allowed {
		writeError(w, 403, "contact_blocked")
		return false
	}
	return true
}

func registerContacts(mux *http.ServeMux, d Dependencies) {
	mux.Handle("GET /api/v1/chats/{id}/contact", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validUUID(id) {
			writeError(w, 404, "not_found")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		var v contactProfile
		var hasAvatar, disabled, blocked bool
		var lastSeen *time.Time
		err := d.DB.QueryRow(r.Context(), `SELECT u.id::text,u.login,u.display_name,u.avatar_key IS NOT NULL,u.disabled,u.last_seen_at,
 EXISTS(SELECT 1 FROM user_blocks b WHERE b.source_user_id=$2 AND b.target_user_id=u.id),
 EXISTS(SELECT 1 FROM user_blocks b WHERE (b.source_user_id=$2 AND b.target_user_id=u.id) OR (b.source_user_id=u.id AND b.target_user_id=$2))
 FROM chats c JOIN chat_members cm ON cm.chat_id=c.id AND cm.user_id=$2
 JOIN users u ON u.id=CASE WHEN c.direct_user_low=$2 THEN c.direct_user_high ELSE c.direct_user_low END
 WHERE c.id=$1 AND c.kind='direct' AND c.deleted_at IS NULL`, id, p.UserID).Scan(&v.ID, &v.Login, &v.DisplayName, &hasAvatar, &disabled, &lastSeen, &v.BlockedByMe, &blocked)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		v.CanMessage = !disabled && !blocked
		v.CanCall = v.CanMessage
		if hasAvatar && !disabled {
			v.AvatarURL = avatarURL(v.ID)
		}
		if !blocked && !disabled {
			v.Presence = &peerPresence{Online: d.Events != nil && d.Events.HasUser(v.ID), LastSeenAt: lastSeen}
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, v)
	})))
	mux.Handle("PUT /api/v1/users/{userID}/block", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		target := r.PathValue("userID")
		if !validUUID(target) || strings.EqualFold(target, p.UserID) {
			writeError(w, 400, "invalid_user")
			return
		}
		var body struct {
			Blocked *bool `json:"blocked"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Blocked == nil {
			writeError(w, 400, "invalid_block")
			return
		}
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		if err = lockContactPair(r.Context(), tx, p.UserID, target); err != nil {
			writeError(w, 500, "internal")
			return
		}
		var exists bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, target).Scan(&exists); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if !exists {
			writeError(w, 404, "not_found")
			return
		}
		var chatID string
		err = tx.QueryRow(r.Context(), `SELECT id::text FROM chats WHERE kind='direct' AND direct_user_low=LEAST($1::uuid,$2::uuid) AND direct_user_high=GREATEST($1::uuid,$2::uuid) AND deleted_at IS NULL FOR UPDATE`, p.UserID, target).Scan(&chatID)
		if err != nil && err != pgx.ErrNoRows {
			writeError(w, 500, "internal")
			return
		}
		var changed int64
		if *body.Blocked {
			result, e := tx.Exec(r.Context(), `INSERT INTO user_blocks(source_user_id,target_user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, p.UserID, target)
			err = e
			changed = result.RowsAffected()
		} else {
			result, e := tx.Exec(r.Context(), `DELETE FROM user_blocks WHERE source_user_id=$1 AND target_user_id=$2`, p.UserID, target)
			err = e
			changed = result.RowsAffected()
		}
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		if chatID != "" && *body.Blocked {
			_, err = tx.Exec(r.Context(), `WITH stopped AS (UPDATE calls SET state=CASE WHEN state='ringing' THEN 'cancelled' ELSE 'ended' END,ended_at=now() WHERE chat_id=$1 AND state IN ('ringing','accepted') RETURNING id,chat_id,state)
 INSERT INTO events(chat_id,kind,payload) SELECT chat_id,'call_'||state,jsonb_build_object('call_id',id::text,'state',state) FROM stopped`, chatID)
			if err == nil {
				_, err = tx.Exec(r.Context(), `DELETE FROM push_jobs WHERE kind='voip' AND payload->>'call_id' IN (SELECT id::text FROM calls WHERE chat_id=$1)`, chatID)
			}
		}
		if err == nil && chatID != "" && changed > 0 {
			_, err = tx.Exec(r.Context(), `INSERT INTO events(chat_id,kind,payload) VALUES($1,'contact_updated',jsonb_build_object('user_id',$2::text))`, chatID, p.UserID)
		}
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		w.WriteHeader(204)
	})))
}
