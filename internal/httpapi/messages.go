package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"familychat/server/internal/auth"
	"github.com/jackc/pgx/v5"
)

type messageView struct {
	ID              string `json:"id"`
	ChatID          string `json:"chat_id"`
	Seq             int64  `json:"seq"`
	SenderID        string `json:"sender_id"`
	ClientMessageID string `json:"client_message_id"`
	Text            string `json:"text"`
	CreatedAt       string `json:"created_at"`
}

func registerMessages(mux *http.ServeMux, d Dependencies) {
	protected := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, require(d.Auth, fn)) }
	protected("POST /api/v1/chats/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ClientMessageID string   `json:"client_message_id"`
			Text            string   `json:"text"`
			AttachmentIDs   []string `json:"attachment_ids"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if !validUUID(body.ClientMessageID) || utf8.RuneCountInString(body.Text) > 16000 || (strings.TrimSpace(body.Text) == "" && len(body.AttachmentIDs) == 0) || len(body.AttachmentIDs) > 10 {
			writeError(w, 400, "invalid_message")
			return
		}
		chatID := r.PathValue("id")
		p, _ := auth.PrincipalFrom(r.Context())
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		if _, _, err = chatRole(r, tx, chatID, p.UserID); err != nil {
			writeError(w, 404, "not_found")
			return
		}
		var v messageView
		var existingText string
		err = tx.QueryRow(r.Context(), `SELECT id::text,chat_id::text,seq,sender_id::text,client_message_id::text,body,created_at::text FROM messages WHERE chat_id=$1 AND sender_id=$2 AND client_message_id=$3`, chatID, p.UserID, body.ClientMessageID).Scan(&v.ID, &v.ChatID, &v.Seq, &v.SenderID, &v.ClientMessageID, &existingText, &v.CreatedAt)
		if err == nil {
			if existingText != body.Text {
				writeError(w, 409, "message_id_conflict")
				return
			}
			v.Text = existingText
			writeJSON(w, 200, v)
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 500, "internal")
			return
		}
		if len(body.AttachmentIDs) > 0 {
			seen := map[string]bool{}
			for _, id := range body.AttachmentIDs {
				if !validUUID(id) || seen[id] {
					writeError(w, 400, "invalid_attachment")
					return
				}
				seen[id] = true
				var valid bool
				err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM attachments a WHERE a.id=$1 AND a.chat_id=$2 AND a.uploader_id=$3 AND NOT EXISTS(SELECT 1 FROM message_attachments ma WHERE ma.attachment_id=a.id))`, id, chatID, p.UserID).Scan(&valid)
				if err != nil || !valid {
					writeError(w, 404, "attachment_not_found")
					return
				}
			}
		}
		var seq int64
		if err = tx.QueryRow(r.Context(), `UPDATE chats SET next_seq=next_seq+1 WHERE id=$1 RETURNING next_seq-1`, chatID).Scan(&seq); err != nil {
			writeError(w, 500, "internal")
			return
		}
		err = tx.QueryRow(r.Context(), `INSERT INTO messages(chat_id,seq,sender_id,client_message_id,body) VALUES($1,$2,$3,$4,$5) RETURNING id::text,created_at::text`, chatID, seq, p.UserID, body.ClientMessageID, body.Text).Scan(&v.ID, &v.CreatedAt)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		for _, id := range body.AttachmentIDs {
			if _, err = tx.Exec(r.Context(), `INSERT INTO message_attachments(message_id,attachment_id) VALUES($1,$2)`, v.ID, id); err != nil {
				writeError(w, 409, "attachment_conflict")
				return
			}
		}
		if _, err = tx.Exec(r.Context(), `INSERT INTO events(chat_id,kind,payload) VALUES($1,'message',jsonb_build_object('message_id',$2::text,'seq',$3::bigint))`, chatID, v.ID, seq); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		v.ChatID = chatID
		v.Seq = seq
		v.SenderID = p.UserID
		v.ClientMessageID = body.ClientMessageID
		v.Text = body.Text
		writeJSON(w, 201, v)
	})
	protected("GET /api/v1/chats/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validUUID(id) {
			writeError(w, 404, "not_found")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		after, before := r.URL.Query().Get("after"), r.URL.Query().Get("before")
		if after != "" && before != "" {
			writeError(w, 400, "invalid_cursor")
			return
		}
		var cursor int64
		if after != "" || before != "" {
			value := after
			if value == "" {
				value = before
			}
			var err error
			cursor, err = strconv.ParseInt(value, 10, 64)
			if err != nil || cursor < 0 {
				writeError(w, 400, "invalid_cursor")
				return
			}
		}
		var rows pgx.Rows
		var err error
		if after != "" {
			rows, err = d.DB.Query(r.Context(), `SELECT m.id::text,m.chat_id::text,m.seq,m.sender_id::text,m.client_message_id::text,m.body,m.created_at::text
				FROM messages m JOIN chat_members cm ON cm.chat_id=m.chat_id JOIN chats c ON c.id=m.chat_id
				WHERE m.chat_id=$1 AND cm.user_id=$2 AND c.deleted_at IS NULL AND m.seq>$3 ORDER BY m.seq ASC LIMIT $4`, id, p.UserID, cursor, parseLimit(r))
		} else {
			if before == "" {
				cursor = 9223372036854775807
			}
			rows, err = d.DB.Query(r.Context(), `SELECT m.id::text,m.chat_id::text,m.seq,m.sender_id::text,m.client_message_id::text,m.body,m.created_at::text
				FROM messages m JOIN chat_members cm ON cm.chat_id=m.chat_id JOIN chats c ON c.id=m.chat_id
				WHERE m.chat_id=$1 AND cm.user_id=$2 AND c.deleted_at IS NULL AND m.seq<$3 ORDER BY m.seq DESC LIMIT $4`, id, p.UserID, cursor, parseLimit(r))
		}
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer rows.Close()
		messages := []messageView{}
		for rows.Next() {
			var v messageView
			if err := rows.Scan(&v.ID, &v.ChatID, &v.Seq, &v.SenderID, &v.ClientMessageID, &v.Text, &v.CreatedAt); err != nil {
				writeError(w, 500, "internal")
				return
			}
			messages = append(messages, v)
		}
		if rows.Err() != nil {
			writeError(w, 500, "internal")
			return
		}
		// An empty page still must not reveal whether an inaccessible chat exists.
		var allowed bool
		if err := d.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM chat_members cm JOIN chats c ON c.id=cm.chat_id WHERE cm.chat_id=$1 AND cm.user_id=$2 AND c.deleted_at IS NULL)`, id, p.UserID).Scan(&allowed); err != nil || !allowed {
			writeError(w, 404, "not_found")
			return
		}
		writeJSON(w, 200, map[string]any{"messages": messages})
	})
	protected("POST /api/v1/chats/{id}/read", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Seq int64 `json:"seq"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Seq < 0 {
			writeError(w, 400, "invalid_seq")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		id := r.PathValue("id")
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		if _, _, err = chatRole(r, tx, id, p.UserID); err != nil {
			writeError(w, 404, "not_found")
			return
		}
		var readSeq int64
		if err = tx.QueryRow(r.Context(), `UPDATE chat_members SET read_seq=GREATEST(read_seq,LEAST($3,(SELECT next_seq-1 FROM chats WHERE id=$1))) WHERE chat_id=$1 AND user_id=$2 RETURNING read_seq`, id, p.UserID, body.Seq).Scan(&readSeq); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 200, map[string]int64{"read_seq": readSeq})
	})
	protected("GET /api/v1/chats/{id}/search", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validUUID(id) {
			writeError(w, 404, "not_found")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if utf8.RuneCountInString(q) < 2 || utf8.RuneCountInString(q) > 100 {
			writeError(w, 400, "invalid_query")
			return
		}
		rows, err := d.DB.Query(r.Context(), `SELECT m.id::text,m.chat_id::text,m.seq,m.sender_id::text,m.client_message_id::text,m.body,m.created_at::text
			FROM messages m JOIN chats c ON c.id=m.chat_id JOIN chat_members cm ON cm.chat_id=c.id
			WHERE m.chat_id=$1 AND cm.user_id=$2 AND c.deleted_at IS NULL AND
			(to_tsvector('russian',m.body) @@ plainto_tsquery('russian',$3) OR m.body ILIKE '%'||$3||'%' OR
			 EXISTS(SELECT 1 FROM message_attachments ma JOIN attachments a ON a.id=ma.attachment_id WHERE ma.message_id=m.id AND a.filename ILIKE '%'||$3||'%'))
			ORDER BY m.seq DESC LIMIT $4`, id, p.UserID, q, parseLimit(r))
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer rows.Close()
		messages := []messageView{}
		for rows.Next() {
			var v messageView
			if err := rows.Scan(&v.ID, &v.ChatID, &v.Seq, &v.SenderID, &v.ClientMessageID, &v.Text, &v.CreatedAt); err != nil {
				writeError(w, 500, "internal")
				return
			}
			messages = append(messages, v)
		}
		if rows.Err() != nil {
			writeError(w, 500, "internal")
			return
		}
		var allowed bool
		if err := d.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM chat_members cm JOIN chats c ON c.id=cm.chat_id WHERE cm.chat_id=$1 AND cm.user_id=$2 AND c.deleted_at IS NULL)`, id, p.UserID).Scan(&allowed); err != nil || !allowed {
			writeError(w, 404, "not_found")
			return
		}
		writeJSON(w, 200, map[string]any{"messages": messages})
	})
}
