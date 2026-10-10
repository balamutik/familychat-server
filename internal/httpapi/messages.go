package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"familychat/server/internal/auth"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type messageView struct {
	Encrypted       bool          `json:"encrypted"`
	ID              string        `json:"id"`
	ChatID          string        `json:"chat_id"`
	Seq             int64         `json:"seq"`
	SenderID        string        `json:"sender_id"`
	ClientMessageID string        `json:"client_message_id"`
	Text            string        `json:"text"`
	CreatedAt       string        `json:"created_at"`
	Attachments     []fileMeta    `json:"attachments"`
	Receipts        []receiptView `json:"receipts"`
	Delivery        deliveryView  `json:"delivery"`
}

type receiptView struct {
	UserID    string `json:"user_id"`
	Delivered bool   `json:"delivered"`
	Read      bool   `json:"read"`
}

type deliveryView struct {
	RecipientCount int    `json:"recipient_count"`
	DeliveredCount int    `json:"delivered_count"`
	ReadCount      int    `json:"read_count"`
	Status         string `json:"status"`
}

func registerMessages(mux *http.ServeMux, d Dependencies) {
	protected := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, require(d.Auth, fn)) }
	protected("POST /api/v1/chats/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Encrypted       bool     `json:"encrypted"`
			ClientMessageID string   `json:"client_message_id"`
			Text            string   `json:"text"`
			AttachmentIDs   []string `json:"attachment_ids"`
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
		if !validUUID(body.ClientMessageID) || (!body.Encrypted && (utf8.RuneCountInString(body.Text) > 16000 || (strings.TrimSpace(body.Text) == "" && len(body.AttachmentIDs) == 0))) || len(body.AttachmentIDs) > 10 {
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
		err = tx.QueryRow(r.Context(), `SELECT id::text,chat_id::text,seq,sender_id::text,client_message_id::text,body,encrypted,created_at::text FROM messages WHERE chat_id=$1 AND sender_id=$2 AND client_message_id=$3`, chatID, p.UserID, body.ClientMessageID).Scan(&v.ID, &v.ChatID, &v.Seq, &v.SenderID, &v.ClientMessageID, &existingText, &v.Encrypted, &v.CreatedAt)
		if err == nil {
			rows, queryErr := tx.Query(r.Context(), `SELECT attachment_id::text FROM message_attachments WHERE message_id=$1 ORDER BY attachment_id`, v.ID)
			if queryErr != nil {
				writeError(w, 500, "internal")
				return
			}
			existingIDs := []string{}
			for rows.Next() {
				var id string
				if scanErr := rows.Scan(&id); scanErr != nil {
					rows.Close()
					writeError(w, 500, "internal")
					return
				}
				existingIDs = append(existingIDs, id)
			}
			queryErr = rows.Err()
			rows.Close()
			if queryErr != nil {
				writeError(w, 500, "internal")
				return
			}
			requestedIDs := append([]string(nil), body.AttachmentIDs...)
			sort.Strings(requestedIDs)
			sameIDs := len(existingIDs) == len(requestedIDs)
			if sameIDs {
				for i := range existingIDs {
					if existingIDs[i] != requestedIDs[i] {
						sameIDs = false
						break
					}
				}
			}
			if v.Encrypted != body.Encrypted || (!body.Encrypted && existingText != body.Text) || !sameIDs {
				writeError(w, 409, "message_id_conflict")
				return
			}
			v.Text = existingText
			if err := attachMetadata(r.Context(), d.DB, []*messageView{&v}); err != nil {
				writeError(w, 500, "internal")
				return
			}
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
				var lockedID string
				err = tx.QueryRow(r.Context(), `SELECT a.id::text FROM attachments a CROSS JOIN settings s WHERE a.id=$1 AND a.chat_id=$2 AND a.uploader_id=$3
					AND a.purged_at IS NULL AND (s.retention_days=0 OR a.created_at>now()-s.retention_days*interval '1 day')
					AND a.created_at>now()-interval '1 day'
					AND NOT EXISTS(SELECT 1 FROM message_attachments ma WHERE ma.attachment_id=a.id)
					FOR UPDATE OF a`, id, chatID, p.UserID).Scan(&lockedID)
				if err != nil {
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
		err = tx.QueryRow(r.Context(), `INSERT INTO messages(chat_id,seq,sender_id,client_message_id,body,encrypted) VALUES($1,$2,$3,$4,$5,$6) RETURNING id::text,created_at::text`, chatID, seq, p.UserID, body.ClientMessageID, body.Text, body.Encrypted).Scan(&v.ID, &v.CreatedAt)
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
		if d.PushEnabled {
			_, err = tx.Exec(r.Context(), `INSERT INTO push_jobs(device_id,kind,payload)
			SELECT d.id,'alert',jsonb_build_object('aps',jsonb_build_object('alert',jsonb_build_object('title','FamilyChat','body','Новое сообщение'),'sound','default'),
			'server_id',d.client_server_id::text,'chat_id',$1::text,'message_id',$2::text)
			FROM push_devices d JOIN chat_members cm ON cm.user_id=d.user_id JOIN sessions s ON s.id=d.session_id
			WHERE cm.chat_id=$1::uuid AND d.user_id<>$3 AND d.alert_token IS NOT NULL
			AND s.revoked_at IS NULL AND s.expires_at>now()`, chatID, v.ID, p.UserID)
		}
		if err != nil {
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
		v.Encrypted = body.Encrypted
		if err := attachMetadata(r.Context(), d.DB, []*messageView{&v}); err != nil {
			writeError(w, 500, "internal")
			return
		}
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
			rows, err = d.DB.Query(r.Context(), `SELECT m.id::text,m.chat_id::text,m.seq,m.sender_id::text,m.client_message_id::text,m.body,m.encrypted,m.created_at::text
				FROM messages m JOIN chat_members cm ON cm.chat_id=m.chat_id JOIN chats c ON c.id=m.chat_id
				WHERE m.chat_id=$1 AND cm.user_id=$2 AND c.deleted_at IS NULL AND m.seq>$3 ORDER BY m.seq ASC LIMIT $4`, id, p.UserID, cursor, parseLimit(r))
		} else {
			if before == "" {
				cursor = 9223372036854775807
			}
			rows, err = d.DB.Query(r.Context(), `SELECT m.id::text,m.chat_id::text,m.seq,m.sender_id::text,m.client_message_id::text,m.body,m.encrypted,m.created_at::text
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
			if err := rows.Scan(&v.ID, &v.ChatID, &v.Seq, &v.SenderID, &v.ClientMessageID, &v.Text, &v.Encrypted, &v.CreatedAt); err != nil {
				writeError(w, 500, "internal")
				return
			}
			messages = append(messages, v)
		}
		if rows.Err() != nil {
			writeError(w, 500, "internal")
			return
		}
		rows.Close()
		// An empty page still must not reveal whether an inaccessible chat exists.
		var allowed bool
		if err := d.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM chat_members cm JOIN chats c ON c.id=cm.chat_id WHERE cm.chat_id=$1 AND cm.user_id=$2 AND c.deleted_at IS NULL)`, id, p.UserID).Scan(&allowed); err != nil || !allowed {
			writeError(w, 404, "not_found")
			return
		}
		if err := attachMetadataList(r.Context(), d.DB, messages); err != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 200, map[string]any{"messages": messages})
	})
	protected("POST /api/v1/chats/{id}/delivered", func(w http.ResponseWriter, r *http.Request) { markReceipt(w, r, d, false) })
	protected("POST /api/v1/chats/{id}/read", func(w http.ResponseWriter, r *http.Request) { markReceipt(w, r, d, true) })
	protected("GET /api/v1/chats/{id}/search", func(w http.ResponseWriter, r *http.Request) {
		if d.ContentKey != nil {
			writeError(w, 410, "search_on_device")
			return
		}
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
		rows, err := d.DB.Query(r.Context(), `SELECT m.id::text,m.chat_id::text,m.seq,m.sender_id::text,m.client_message_id::text,m.body,m.encrypted,m.created_at::text
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
			if err := rows.Scan(&v.ID, &v.ChatID, &v.Seq, &v.SenderID, &v.ClientMessageID, &v.Text, &v.Encrypted, &v.CreatedAt); err != nil {
				writeError(w, 500, "internal")
				return
			}
			messages = append(messages, v)
		}
		if rows.Err() != nil {
			writeError(w, 500, "internal")
			return
		}
		rows.Close()
		var allowed bool
		if err := d.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM chat_members cm JOIN chats c ON c.id=cm.chat_id WHERE cm.chat_id=$1 AND cm.user_id=$2 AND c.deleted_at IS NULL)`, id, p.UserID).Scan(&allowed); err != nil || !allowed {
			writeError(w, 404, "not_found")
			return
		}
		if err := attachMetadataList(r.Context(), d.DB, messages); err != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 200, map[string]any{"messages": messages})
	})
}

func markReceipt(w http.ResponseWriter, r *http.Request, d Dependencies, read bool) {
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
	var oldDelivered, oldRead, deliveredSeq, readSeq int64
	if err = tx.QueryRow(r.Context(), `SELECT delivered_seq,read_seq FROM chat_members WHERE chat_id=$1 AND user_id=$2`, id, p.UserID).Scan(&oldDelivered, &oldRead); err != nil {
		writeError(w, 500, "internal")
		return
	}
	if err = tx.QueryRow(r.Context(), `UPDATE chat_members SET
			delivered_seq=GREATEST(delivered_seq,LEAST($3,(SELECT next_seq-1 FROM chats WHERE id=$1))),
			read_seq=CASE WHEN $4 THEN GREATEST(read_seq,LEAST($3,(SELECT next_seq-1 FROM chats WHERE id=$1))) ELSE read_seq END
			WHERE chat_id=$1 AND user_id=$2 RETURNING delivered_seq,read_seq`, id, p.UserID, body.Seq, read).Scan(&deliveredSeq, &readSeq); err != nil {
		writeError(w, 500, "internal")
		return
	}
	if deliveredSeq != oldDelivered || readSeq != oldRead {
		if _, err = tx.Exec(r.Context(), `INSERT INTO events(chat_id,kind,payload) VALUES($1,'receipt',jsonb_build_object('user_id',$2::text,'delivered_seq',$3::bigint,'read_seq',$4::bigint))`, id, p.UserID, deliveredSeq, readSeq); err != nil {
			writeError(w, 500, "internal")
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "internal")
		return
	}
	writeJSON(w, 200, map[string]int64{"delivered_seq": deliveredSeq, "read_seq": readSeq})
}

func attachMetadataList(ctx context.Context, db *pgxpool.Pool, messages []messageView) error {
	refs := make([]*messageView, 0, len(messages))
	for i := range messages {
		refs = append(refs, &messages[i])
	}
	return attachMetadata(ctx, db, refs)
}

func attachMetadata(ctx context.Context, db *pgxpool.Pool, messages []*messageView) error {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]string, 0, len(messages))
	byID := make(map[string]*messageView, len(messages))
	for _, v := range messages {
		v.Attachments = []fileMeta{}
		ids = append(ids, v.ID)
		byID[v.ID] = v
	}
	rows, err := db.Query(ctx, `SELECT ma.message_id::text,a.id::text,a.chat_id::text,a.filename,a.content_type,a.size_bytes,a.preview_state,a.encrypted,
		(a.purged_at IS NULL AND (s.retention_days=0 OR a.created_at>now()-s.retention_days*interval '1 day'))
		FROM message_attachments ma JOIN attachments a ON a.id=ma.attachment_id CROSS JOIN settings s
		WHERE ma.message_id=ANY($1::uuid[]) ORDER BY ma.message_id,a.id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		var a fileMeta
		if err := rows.Scan(&messageID, &a.ID, &a.ChatID, &a.Filename, &a.ContentType, &a.Size, &a.PreviewState, &a.Encrypted, &a.Available); err != nil {
			return err
		}
		if v := byID[messageID]; v != nil {
			v.Attachments = append(v.Attachments, a)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, v := range messages {
		v.Receipts = []receiptView{}
		v.Delivery = deliveryView{Status: "sent"}
	}
	receipts, err := db.Query(ctx, `SELECT m.id::text,cm.user_id::text,cm.delivered_seq>=m.seq,cm.read_seq>=m.seq
		FROM messages m JOIN chat_members cm ON cm.chat_id=m.chat_id AND cm.user_id<>m.sender_id AND cm.joined_at<=m.created_at
		WHERE m.id=ANY($1::uuid[]) ORDER BY m.id,cm.user_id`, ids)
	if err != nil {
		return err
	}
	defer receipts.Close()
	for receipts.Next() {
		var messageID string
		var receipt receiptView
		if err := receipts.Scan(&messageID, &receipt.UserID, &receipt.Delivered, &receipt.Read); err != nil {
			return err
		}
		if v := byID[messageID]; v != nil {
			v.Receipts = append(v.Receipts, receipt)
			v.Delivery.RecipientCount++
			if receipt.Delivered {
				v.Delivery.DeliveredCount++
			}
			if receipt.Read {
				v.Delivery.ReadCount++
			}
		}
	}
	if err := receipts.Err(); err != nil {
		return err
	}
	for _, v := range messages {
		if v.Delivery.RecipientCount > 0 && v.Delivery.ReadCount == v.Delivery.RecipientCount {
			v.Delivery.Status = "read"
		} else if v.Delivery.RecipientCount > 0 && v.Delivery.DeliveredCount == v.Delivery.RecipientCount {
			v.Delivery.Status = "delivered"
		}
	}
	return nil
}
