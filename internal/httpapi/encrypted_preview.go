package httpapi

import (
	"bytes"
	"io"
	"net/http"

	"familychat/server/internal/auth"
	"familychat/server/internal/contentcrypto"
)

func uploadEncryptedPreview(w http.ResponseWriter, r *http.Request, d Dependencies) {
	if d.ContentKey == nil || d.Objects == nil {
		writeError(w, 503, "encryption_unavailable")
		return
	}
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
	// Serialize quota accounting with file and avatar uploads.
	if _, err = tx.Exec(r.Context(), `SELECT id FROM users WHERE id=$1 FOR UPDATE`, p.UserID); err != nil {
		writeError(w, 500, "internal")
		return
	}
	var chat, old string
	err = tx.QueryRow(r.Context(), `SELECT a.chat_id::text,COALESCE(a.preview_key,'') FROM attachments a JOIN chat_members cm ON cm.chat_id=a.chat_id JOIN chats c ON c.id=a.chat_id CROSS JOIN settings s
 WHERE a.id=$1 AND a.uploader_id=$2 AND cm.user_id=$2 AND a.encrypted AND a.purged_at IS NULL AND c.deleted_at IS NULL
 AND (s.retention_days=0 OR a.created_at>now()-s.retention_days*interval '1 day') FOR UPDATE OF a`, id, p.UserID).Scan(&chat, &old)
	if err != nil {
		writeError(w, 404, "not_found")
		return
	}
	if old != "" {
		writeJSON(w, 200, map[string]string{"preview_state": "ready"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, contentcrypto.StoredSize(1<<20)))
	if err != nil || len(raw) < contentcrypto.HeaderSize {
		writeError(w, 400, "invalid_encrypted_content")
		return
	}
	size, err := d.ContentKey.HeaderSize(raw[:contentcrypto.HeaderSize])
	if err != nil || size <= 0 || size > 1<<20 || int64(len(raw)) != contentcrypto.StoredSize(size) {
		writeError(w, 400, "invalid_encrypted_content")
		return
	}
	quota := d.UserQuotaBytes
	if quota <= 0 {
		quota = 10 << 30
	}
	var used int64
	if err := tx.QueryRow(r.Context(), `SELECT
        COALESCE((SELECT sum(size_bytes+preview_size_bytes+CASE WHEN encrypted THEN 80+16*((size_bytes+1048575)/1048576) ELSE 0 END) FROM attachments WHERE uploader_id=$1 AND purged_at IS NULL),0)
        +COALESCE((SELECT avatar_size_bytes FROM users WHERE id=$1),0)
        +COALESCE((SELECT sum(reserved_bytes) FROM upload_reservations WHERE user_id=$1 AND state='uploading' AND expires_at>now()),0)`, p.UserID).Scan(&used); err != nil {
		writeError(w, 500, "internal")
		return
	}
	if used > quota-int64(len(raw)) {
		writeError(w, 413, "quota_exceeded")
		return
	}
	key, err := newObjectKey()
	if err != nil {
		writeError(w, 500, "internal")
		return
	}
	// Reserve cleanup outside the transaction so a crash during S3 upload
	// cannot leave an untracked object. The worker waits for the upload lease.
	if _, err = d.DB.Exec(r.Context(), `INSERT INTO encryption_gc(object_key,not_before) VALUES($1,now()+interval '1 hour')`, key); err != nil {
		writeError(w, 500, "internal")
		return
	}
	if err := d.Objects.Put(r.Context(), key, bytes.NewReader(raw), int64(len(raw)), "application/octet-stream"); err != nil {
		writeError(w, 502, "upload_failed")
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = d.Objects.Delete(r.Context(), key)
		}
	}()
	if _, err = tx.Exec(r.Context(), `UPDATE attachments SET preview_key=$2,preview_state='ready',preview_size_bytes=$3 WHERE id=$1`, id, key, len(raw)); err != nil {
		writeError(w, 500, "internal")
		return
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM encryption_gc WHERE object_key=$1`, key); err != nil {
		writeError(w, 500, "internal")
		return
	}

	if _, err = tx.Exec(r.Context(), `INSERT INTO events(chat_id,kind,payload) VALUES($1,'preview',jsonb_build_object('attachment_id',$2::text,'state','ready'))`, chat, id); err != nil {
		writeError(w, 500, "internal")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "internal")
		return
	}
	committed = true
	writeJSON(w, 200, map[string]string{"preview_state": "ready"})
}
