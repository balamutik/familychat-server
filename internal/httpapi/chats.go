package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"familychat/server/internal/auth"
	"github.com/jackc/pgx/v5"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func validUUID(id string) bool { return uuidPattern.MatchString(id) }

type peerPresence struct {
	Online     bool       `json:"online"`
	LastSeenAt *time.Time `json:"last_seen_at"`
}

type chatView struct {
	Presence           *peerPresence `json:"presence,omitempty"`
	peerID             string
	peerDisabled       bool
	ID                 string  `json:"id"`
	Kind               string  `json:"kind"`
	Title              string  `json:"title"`
	Role               string  `json:"role,omitempty"`
	AvatarURL          string  `json:"avatar_url,omitempty"`
	UnreadCount        int64   `json:"unread_count"`
	LastMessagePreview *string `json:"last_message_preview,omitempty"`
	LastMessageAt      *string `json:"last_message_at,omitempty"`
	LastMessageSender  *string `json:"last_message_sender,omitempty"`
}

func (v *chatView) setPresence(d Dependencies, lastSeen *time.Time) {
	if v.Kind != "direct" {
		return
	}
	v.Presence = &peerPresence{LastSeenAt: lastSeen,
		Online: !v.peerDisabled && d.Events != nil && d.Events.HasUser(v.peerID)}
}

func registerChats(mux *http.ServeMux, d Dependencies) {
	protected := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, require(d.Auth, fn)) }
	protected("POST /api/v1/chats/direct", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UserID string `json:"user_id"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		if !validUUID(body.UserID) || body.UserID == p.UserID {
			writeError(w, 400, "invalid_user")
			return
		}
		var active bool
		var peerLogin string
		var peerHasAvatar bool
		var peerLastSeen *time.Time
		if err := d.DB.QueryRow(r.Context(), `SELECT NOT disabled,login,avatar_key IS NOT NULL,last_seen_at FROM users WHERE id=$1`, body.UserID).Scan(&active, &peerLogin, &peerHasAvatar, &peerLastSeen); err != nil || !active {
			writeError(w, 404, "not_found")
			return
		}
		low, high := p.UserID, body.UserID
		if low > high {
			low, high = high, low
		}
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		var id string
		created := true
		err = tx.QueryRow(r.Context(), `INSERT INTO chats(kind,direct_user_low,direct_user_high) VALUES('direct',$1,$2)
			ON CONFLICT (direct_user_low,direct_user_high) WHERE kind='direct' DO NOTHING RETURNING id::text`, low, high).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			created = false
			err = tx.QueryRow(r.Context(), `SELECT id::text FROM chats WHERE kind='direct' AND direct_user_low=$1 AND direct_user_high=$2`, low, high).Scan(&id)
		}
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		if created {
			if _, err = tx.Exec(r.Context(), `INSERT INTO chat_members(chat_id,user_id,role) VALUES($1,$2,'member'),($1,$3,'member')`, id, p.UserID, body.UserID); err != nil {
				writeError(w, 500, "internal")
				return
			}
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		status := 200
		if created {
			status = 201
		}
		v := chatView{ID: id, Kind: "direct", Title: peerLogin, Role: "member", peerID: body.UserID}
		v.setPresence(d, peerLastSeen)
		if peerHasAvatar {
			v.AvatarURL = avatarURL(body.UserID)
		}
		if !created {
			if err := d.DB.QueryRow(r.Context(), `SELECT count(*) FROM messages msg JOIN chat_members cm ON cm.chat_id=msg.chat_id
				WHERE cm.chat_id=$1 AND cm.user_id=$2 AND msg.seq>cm.read_seq AND msg.sender_id<>$2`, id, p.UserID).Scan(&v.UnreadCount); err != nil {
				writeError(w, 500, "internal")
				return
			}
		}
		writeJSON(w, status, v)
	})
	protected("POST /api/v1/chats", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		body.Title = strings.TrimSpace(body.Title)
		if body.Title == "" || len([]rune(body.Title)) > 100 {
			writeError(w, 400, "invalid_title")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		var id string
		if err = tx.QueryRow(r.Context(), `INSERT INTO chats(kind,title) VALUES('group',$1) RETURNING id::text`, body.Title).Scan(&id); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if _, err = tx.Exec(r.Context(), `INSERT INTO chat_members(chat_id,user_id,role) VALUES($1,$2,'owner')`, id, p.UserID); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 201, chatView{ID: id, Kind: "group", Title: body.Title, Role: "owner"})
	})
	protected("GET /api/v1/chats", func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		rows, err := d.DB.Query(r.Context(), `SELECT c.id::text,c.kind,CASE WHEN c.kind='direct' THEN peer.login ELSE c.title END,m.role,
			CASE WHEN peer.avatar_key IS NOT NULL AND NOT peer.disabled THEN '/api/v1/users/'||peer.id::text||'/avatar' ELSE '' END,
			(SELECT count(*) FROM messages msg WHERE msg.chat_id=c.id AND msg.seq>m.read_seq AND msg.sender_id<>$1),
			recent.preview,recent.created_at,recent.sender_login,COALESCE(peer.id::text,''),COALESCE(peer.disabled,false),peer.last_seen_at
			FROM chats c JOIN chat_members m ON m.chat_id=c.id
			LEFT JOIN users peer ON peer.id=CASE WHEN c.kind='direct' THEN CASE WHEN c.direct_user_low=$1 THEN c.direct_user_high ELSE c.direct_user_low END ELSE NULL END
			LEFT JOIN LATERAL (SELECT COALESCE(NULLIF(LEFT(BTRIM(msg.body),160),''),
				(SELECT a.filename FROM message_attachments ma JOIN attachments a ON a.id=ma.attachment_id WHERE ma.message_id=msg.id ORDER BY a.created_at LIMIT 1),
				'Вложение') AS preview,msg.created_at::text AS created_at,msg.created_at AS sort_at,sender.login AS sender_login
				FROM messages msg JOIN users sender ON sender.id=msg.sender_id WHERE msg.chat_id=c.id ORDER BY msg.seq DESC LIMIT 1) recent ON true
			WHERE m.user_id=$1 AND c.deleted_at IS NULL ORDER BY COALESCE(recent.sort_at,c.created_at) DESC,c.id DESC LIMIT $2`, p.UserID, parseLimit(r))
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer rows.Close()
		list := []chatView{}
		for rows.Next() {
			var v chatView
			var lastSeen *time.Time
			if err := rows.Scan(&v.ID, &v.Kind, &v.Title, &v.Role, &v.AvatarURL, &v.UnreadCount,
				&v.LastMessagePreview, &v.LastMessageAt, &v.LastMessageSender, &v.peerID, &v.peerDisabled, &lastSeen); err != nil {
				writeError(w, 500, "internal")
				return
			}
			v.setPresence(d, lastSeen)
			list = append(list, v)
		}
		if rows.Err() != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 200, map[string]any{"chats": list})
	})
	protected("GET /api/v1/chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validUUID(id) {
			writeError(w, 404, "not_found")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		var v chatView
		var lastSeen *time.Time
		err := d.DB.QueryRow(r.Context(), `SELECT c.id::text,c.kind,CASE WHEN c.kind='direct' THEN peer.login ELSE c.title END,m.role,
			CASE WHEN peer.avatar_key IS NOT NULL AND NOT peer.disabled THEN '/api/v1/users/'||peer.id::text||'/avatar' ELSE '' END,
			(SELECT count(*) FROM messages msg WHERE msg.chat_id=c.id AND msg.seq>m.read_seq AND msg.sender_id<>$2),
			recent.preview,recent.created_at,recent.sender_login,COALESCE(peer.id::text,''),COALESCE(peer.disabled,false),peer.last_seen_at
			FROM chats c JOIN chat_members m ON m.chat_id=c.id
			LEFT JOIN users peer ON peer.id=CASE WHEN c.kind='direct' THEN CASE WHEN c.direct_user_low=$2 THEN c.direct_user_high ELSE c.direct_user_low END ELSE NULL END
			LEFT JOIN LATERAL (SELECT COALESCE(NULLIF(LEFT(BTRIM(msg.body),160),''),
				(SELECT a.filename FROM message_attachments ma JOIN attachments a ON a.id=ma.attachment_id WHERE ma.message_id=msg.id ORDER BY a.created_at LIMIT 1),
				'Вложение') AS preview,msg.created_at::text AS created_at,sender.login AS sender_login
				FROM messages msg JOIN users sender ON sender.id=msg.sender_id WHERE msg.chat_id=c.id ORDER BY msg.seq DESC LIMIT 1) recent ON true
			WHERE c.id=$1 AND m.user_id=$2 AND c.deleted_at IS NULL`, id, p.UserID).Scan(&v.ID, &v.Kind, &v.Title, &v.Role, &v.AvatarURL, &v.UnreadCount,
			&v.LastMessagePreview, &v.LastMessageAt, &v.LastMessageSender, &v.peerID, &v.peerDisabled, &lastSeen)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		v.setPresence(d, lastSeen)
		writeJSON(w, 200, v)
	})
	protected("GET /api/v1/chats/{id}/members", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validUUID(id) {
			writeError(w, 404, "not_found")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		var allowed bool
		if err := d.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM chat_members cm JOIN chats c ON c.id=cm.chat_id WHERE cm.chat_id=$1 AND cm.user_id=$2 AND c.deleted_at IS NULL)`, id, p.UserID).Scan(&allowed); err != nil || !allowed {
			writeError(w, 404, "not_found")
			return
		}
		rows, err := d.DB.Query(r.Context(), `SELECT u.id::text,u.login,cm.role,u.avatar_key IS NOT NULL AND NOT u.disabled FROM chat_members cm JOIN users u ON u.id=cm.user_id WHERE cm.chat_id=$1 ORDER BY u.login`, id)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer rows.Close()
		type member struct {
			ID        string `json:"id"`
			Login     string `json:"login"`
			Role      string `json:"role"`
			AvatarURL string `json:"avatar_url,omitempty"`
		}
		members := []member{}
		for rows.Next() {
			var m member
			var hasAvatar bool
			if err := rows.Scan(&m.ID, &m.Login, &m.Role, &hasAvatar); err != nil {
				writeError(w, 500, "internal")
				return
			}
			if hasAvatar {
				m.AvatarURL = avatarURL(m.ID)
			}
			members = append(members, m)
		}
		if rows.Err() != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 200, map[string]any{"members": members})
	})
	protected("POST /api/v1/chats/{id}/members", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UserID string `json:"user_id"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if !validUUID(body.UserID) {
			writeError(w, 400, "invalid_user")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		kind, role, err := chatRole(r, tx, r.PathValue("id"), p.UserID)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		if kind != "group" || (role != "owner" && role != "admin") {
			writeError(w, 403, "forbidden")
			return
		}
		var active bool
		if err = tx.QueryRow(r.Context(), `SELECT NOT disabled FROM users WHERE id=$1`, body.UserID).Scan(&active); err != nil || !active {
			writeError(w, 404, "not_found")
			return
		}
		ct, err := tx.Exec(r.Context(), `INSERT INTO chat_members(chat_id,user_id,role) VALUES($1,$2,'member') ON CONFLICT DO NOTHING`, r.PathValue("id"), body.UserID)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		if ct.RowsAffected() == 0 {
			writeError(w, 409, "already_member")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 201, map[string]any{"user_id": body.UserID, "role": "member"})
	})
	protected("DELETE /api/v1/chats/{id}/members/{userID}", func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		target := r.PathValue("userID")
		if !validUUID(target) {
			writeError(w, 404, "not_found")
			return
		}
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		kind, role, err := chatRole(r, tx, r.PathValue("id"), p.UserID)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		if kind != "group" || (role != "owner" && role != "admin") {
			writeError(w, 403, "forbidden")
			return
		}
		var targetRole string
		if err = tx.QueryRow(r.Context(), `SELECT role FROM chat_members WHERE chat_id=$1 AND user_id=$2 FOR UPDATE`, r.PathValue("id"), target).Scan(&targetRole); err != nil {
			writeError(w, 404, "not_found")
			return
		}
		if targetRole == "owner" || (targetRole == "admin" && role != "owner") {
			writeError(w, 403, "forbidden")
			return
		}
		if _, err = tx.Exec(r.Context(), `DELETE FROM chat_members WHERE chat_id=$1 AND user_id=$2`, r.PathValue("id"), target); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if d.Events != nil {
			d.Events.CloseUser(target)
		}
		w.WriteHeader(204)
	})
	protected("POST /api/v1/chats/{id}/leave", func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		kind, role, err := chatRole(r, tx, r.PathValue("id"), p.UserID)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		if kind != "group" || role == "owner" {
			writeError(w, 409, "owner_must_transfer")
			return
		}
		if _, err = tx.Exec(r.Context(), `DELETE FROM chat_members WHERE chat_id=$1 AND user_id=$2`, r.PathValue("id"), p.UserID); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		w.WriteHeader(204)
	})
	protected("POST /api/v1/chats/{id}/owner", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UserID string `json:"user_id"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if !validUUID(body.UserID) {
			writeError(w, 400, "invalid_user")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		kind, role, err := chatRole(r, tx, r.PathValue("id"), p.UserID)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		if kind != "group" || role != "owner" {
			writeError(w, 403, "forbidden")
			return
		}
		if body.UserID == p.UserID {
			writeError(w, 409, "already_owner")
			return
		}
		ct, err := tx.Exec(r.Context(), `UPDATE chat_members SET role='owner' WHERE chat_id=$1 AND user_id=$2`, r.PathValue("id"), body.UserID)
		if err != nil || ct.RowsAffected() != 1 {
			writeError(w, 404, "not_found")
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE chat_members SET role='admin' WHERE chat_id=$1 AND user_id=$2`, r.PathValue("id"), p.UserID); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 200, map[string]string{"owner_id": body.UserID})
	})
	protected("DELETE /api/v1/chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		kind, role, err := chatRole(r, tx, r.PathValue("id"), p.UserID)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		if kind != "group" || role != "owner" {
			writeError(w, 403, "forbidden")
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE chats SET deleted_at=now() WHERE id=$1`, r.PathValue("id")); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if _, err = tx.Exec(r.Context(), `DELETE FROM chat_members WHERE chat_id=$1`, r.PathValue("id")); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		w.WriteHeader(204)
	})
}

func chatRole(r *http.Request, tx pgx.Tx, chatID, userID string) (string, string, error) {
	if !validUUID(chatID) {
		return "", "", errors.New("invalid chat")
	}
	var kind, role string
	if err := tx.QueryRow(r.Context(), `SELECT kind FROM chats WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, chatID).Scan(&kind); err != nil {
		return "", "", err
	}
	if err := tx.QueryRow(r.Context(), `SELECT role FROM chat_members WHERE chat_id=$1 AND user_id=$2 FOR UPDATE`, chatID, userID).Scan(&role); err != nil {
		return "", "", err
	}
	return kind, role, nil
}
