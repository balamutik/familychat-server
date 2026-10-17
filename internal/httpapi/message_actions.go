package httpapi

import (
	"familychat/server/internal/auth"
	"net/http"
	"strings"
	"unicode/utf8"
)

func registerMessageActions(mux *http.ServeMux, d Dependencies) {
	for _, method := range []string{"GET", "PATCH", "DELETE"} {
		mux.Handle(method+" /api/v1/chats/{id}/messages/{messageID}", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			chatID, id := r.PathValue("id"), r.PathValue("messageID")
			if !validUUID(chatID) || !validUUID(id) {
				writeError(w, 404, "not_found")
				return
			}
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
			var deleted, hidden bool
			err = tx.QueryRow(r.Context(), `SELECT id::text,chat_id::text,seq,sender_id::text,client_message_id::text,body,encrypted,created_at::text,deleted_at IS NOT NULL,EXISTS(SELECT 1 FROM message_hidden mh WHERE mh.message_id=messages.id AND mh.user_id=$3) FROM messages WHERE chat_id=$1 AND id=$2 FOR UPDATE`, chatID, id, p.UserID).Scan(&v.ID, &v.ChatID, &v.Seq, &v.SenderID, &v.ClientMessageID, &v.Text, &v.Encrypted, &v.CreatedAt, &deleted, &hidden)
			if err != nil {
				writeError(w, 404, "not_found")
				return
			}
			scope := r.URL.Query().Get("scope")
			if scope == "" {
				scope = "everyone"
			}
			if r.Method == "DELETE" && scope != "self" && scope != "everyone" {
				writeError(w, 400, "invalid_scope")
				return
			}
			if r.Method != "GET" && !(r.Method == "DELETE" && scope == "self") && v.SenderID != p.UserID {
				writeError(w, 404, "not_found")
				return
			}
			if (deleted || hidden) && r.Method != "DELETE" {
				writeError(w, 404, "not_found")
				return
			}
			if r.Method == "GET" {
				if err = tx.Commit(r.Context()); err != nil {
					writeError(w, 500, "internal")
					return
				}
				if err = attachMetadata(r.Context(), d.DB, []*messageView{&v}); err != nil {
					writeError(w, 500, "internal")
					return
				}
				writeJSON(w, 200, v)
				return
			}
			kind := "message_updated"
			if r.Method == "PATCH" {
				var body struct {
					Text      string `json:"text"`
					Encrypted bool   `json:"encrypted"`
				}
				if !decodeJSON(w, r, &body) {
					return
				}
				if d.ContentKey != nil && !body.Encrypted {
					writeError(w, 426, "encryption_required")
					return
				}
				if body.Encrypted && (d.ContentKey == nil || d.ContentKey.ValidateText(body.Text) != nil) {
					writeError(w, 400, "invalid_encrypted_content")
					return
				}
				var hasAttachments bool
				if !body.Encrypted && strings.TrimSpace(body.Text) == "" {
					if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM message_attachments WHERE message_id=$1)`, id).Scan(&hasAttachments); err != nil {
						writeError(w, 500, "internal")
						return
					}
				}
				if !body.Encrypted && (utf8.RuneCountInString(body.Text) > 16000 || (strings.TrimSpace(body.Text) == "" && !hasAttachments)) {
					writeError(w, 400, "invalid_message")
					return
				}
				_, err = tx.Exec(r.Context(), `UPDATE messages SET body=$2,encrypted=$3 WHERE id=$1`, id, body.Text, body.Encrypted)
				v.Text = body.Text
				v.Encrypted = body.Encrypted
			} else {
				if deleted || (scope == "self" && hidden) {
					w.WriteHeader(204)
					return
				}
				if scope == "self" {
					kind = "message_hidden"
					_, err = tx.Exec(r.Context(), `INSERT INTO message_hidden(message_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, p.UserID)
				} else {
					kind = "message_deleted"
					_, err = tx.Exec(r.Context(), `UPDATE messages SET deleted_at=now() WHERE id=$1`, id)
				}
			}
			if err == nil {
				_, err = tx.Exec(r.Context(), `INSERT INTO events(chat_id,kind,payload) VALUES($1,$2,jsonb_build_object('message_id',$3::text,'seq',$4::bigint)||CASE WHEN $2='message_hidden' THEN jsonb_build_object('user_id',$5::text) ELSE '{}'::jsonb END)`, chatID, kind, id, v.Seq, p.UserID)
			}
			if err != nil {
				writeError(w, 500, "internal")
				return
			}
			if err = tx.Commit(r.Context()); err != nil {
				writeError(w, 500, "internal")
				return
			}
			if r.Method == "DELETE" {
				w.WriteHeader(204)
				return
			}
			if err = attachMetadata(r.Context(), d.DB, []*messageView{&v}); err != nil {
				writeError(w, 500, "internal")
				return
			}
			writeJSON(w, 200, v)
		})))
	}
}
