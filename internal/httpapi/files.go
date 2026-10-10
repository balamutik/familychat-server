package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"familychat/server/internal/auth"
	"familychat/server/internal/contentcrypto"
)

var rangePattern = regexp.MustCompile(`^bytes=(\d*)-(\d*)$`)

type fileMeta struct {
	Encrypted    bool   `json:"encrypted"`
	ID           string `json:"id"`
	ChatID       string `json:"chat_id"`
	Filename     string `json:"filename"`
	ContentType  string `json:"content_type"`
	Size         int64  `json:"size"`
	PreviewState string `json:"preview_state"`
	Available    bool   `json:"available"`
	ObjectKey    string `json:"-"`
	PreviewKey   string `json:"-"`
}

// X-File-Name* carries RFC 5987 UTF-8 percent encoding for clients whose HTTP
// stacks reject non-ASCII header values. The original header remains supported.
func uploadFilename(headers http.Header) (string, error) {
	name := headers.Get("X-File-Name")
	if encoded := headers.Get("X-File-Name*"); encoded != "" {
		parts := strings.SplitN(encoded, "'", 3)
		if len(encoded) > 1024 || len(parts) != 3 || !strings.EqualFold(parts[0], "UTF-8") {
			return "", errors.New("invalid filename encoding")
		}
		var err error
		name, err = url.PathUnescape(parts[2])
		if err != nil {
			return "", errors.New("invalid filename encoding")
		}
	}
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == "/" || len(name) > 255 || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\r\n\\") {
		return "", errors.New("invalid filename")
	}
	return name, nil
}

func maxAllowedFileBytes(d Dependencies) int64 {
	if d.MaxFileBytes > 0 {
		return d.MaxFileBytes
	}
	return 250 << 20
}

func registerFiles(mux *http.ServeMux, d Dependencies) {
	protected := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "private, no-store")
			require(d.Auth, fn).ServeHTTP(w, r)
		}))
	}
	protected("POST /api/v1/chats/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		if d.Objects == nil {
			writeError(w, 503, "storage_unavailable")
			return
		}
		chatID := r.PathValue("id")
		if !validUUID(chatID) {
			writeError(w, 404, "not_found")
			return
		}
		encrypted := r.Header.Get("X-Content-Encryption") == contentcrypto.Scheme
		if d.ContentKey != nil && !encrypted {
			writeError(w, 426, "encryption_required")
			return
		}
		size := r.ContentLength
		storedSize := size
		if encrypted {
			var err error
			size, err = strconv.ParseInt(r.Header.Get("X-Plaintext-Size"), 10, 64)
			if err != nil || d.ContentKey == nil || size <= 0 || contentcrypto.StoredSize(size) != storedSize {
				writeError(w, 400, "invalid_encrypted_content")
				return
			}
		}
		limit := maxAllowedFileBytes(d)
		var configuredLimit int64
		if err := d.DB.QueryRow(r.Context(), `SELECT max_file_bytes FROM settings WHERE singleton=true`).Scan(&configuredLimit); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if configuredLimit < limit {
			limit = configuredLimit
		}
		quota := d.UserQuotaBytes
		if quota <= 0 {
			quota = 10 << 30
		}
		if size <= 0 || size > limit {
			writeError(w, 413, "invalid_file_size")
			return
		}
		filename, nameErr := uploadFilename(r.Header)
		if nameErr != nil {
			writeError(w, 400, "invalid_filename")
			return
		}
		contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if len(contentType) > 100 || strings.ContainsAny(contentType, "\r\n") {
			writeError(w, 400, "invalid_content_type")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		key, err := newObjectKey()
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
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
		if _, err = tx.Exec(r.Context(), `SELECT id FROM users WHERE id=$1 FOR UPDATE`, p.UserID); err != nil {
			writeError(w, 500, "internal")
			return
		}
		var used int64
		if err = tx.QueryRow(r.Context(), `SELECT COALESCE((SELECT sum(size_bytes + preview_size_bytes + CASE WHEN encrypted THEN 80+16*((size_bytes+1048575)/1048576) ELSE 0 END) FROM attachments WHERE uploader_id=$1 AND purged_at IS NULL),0)
			+COALESCE((SELECT avatar_size_bytes FROM users WHERE id=$1),0)
			+COALESCE((SELECT sum(reserved_bytes) FROM upload_reservations WHERE user_id=$1 AND state='uploading' AND expires_at>now()),0)`, p.UserID).Scan(&used); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if used > quota-storedSize {
			writeError(w, 413, "quota_exceeded")
			return
		}
		var reservationID string
		if err = tx.QueryRow(r.Context(), `INSERT INTO upload_reservations(user_id,chat_id,object_key,reserved_bytes,state,expires_at) VALUES($1,$2,$3,$4,'uploading',now()+interval '30 minutes') RETURNING id::text`, p.UserID, chatID, key, storedSize).Scan(&reservationID); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		committed := false
		stopRenew := make(chan struct{})
		defer close(stopRenew)
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-stopRenew:
					return
				case <-r.Context().Done():
					return
				case <-ticker.C:
					_, _ = d.DB.Exec(r.Context(), `UPDATE upload_reservations SET expires_at=now()+interval '30 minutes' WHERE id=$1 AND state='uploading'`, reservationID)
				}
			}
		}()
		defer func() {
			if !committed {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_ = d.Objects.Delete(ctx, key)
				_, _ = d.DB.Exec(ctx, `UPDATE upload_reservations SET state='failed' WHERE id=$1 AND state='uploading'`, reservationID)
			}
		}()
		r.Body = http.MaxBytesReader(w, r.Body, storedSize)
		var body io.Reader = r.Body
		objectType := contentType
		if encrypted {
			header := make([]byte, contentcrypto.HeaderSize)
			if _, err = io.ReadFull(r.Body, header); err != nil {
				writeError(w, 400, "invalid_encrypted_content")
				return
			}
			n, err := d.ContentKey.HeaderSize(header)
			if err != nil || n != size {
				writeError(w, 400, "invalid_encrypted_content")
				return
			}
			body = io.MultiReader(bytes.NewReader(header), r.Body)
			objectType = "application/octet-stream"
		}
		if err = d.Objects.Put(r.Context(), key, body, storedSize, objectType); err != nil {
			writeError(w, 502, "upload_failed")
			return
		}
		meta, err := d.Objects.Head(r.Context(), key)
		if err != nil || meta.Size != storedSize {
			writeError(w, 502, "upload_incomplete")
			return
		}
		state := "unsupported"
		if !encrypted && (strings.HasPrefix(contentType, "image/") || strings.HasPrefix(contentType, "video/")) {
			state = "pending"
		}
		finalTx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer finalTx.Rollback(r.Context())
		var reservationState string
		if err = finalTx.QueryRow(r.Context(), `SELECT state FROM upload_reservations WHERE id=$1 FOR UPDATE`, reservationID).Scan(&reservationState); err != nil || reservationState != "uploading" {
			writeError(w, 409, "upload_expired")
			return
		}
		if _, _, err = chatRole(r, finalTx, chatID, p.UserID); err != nil {
			writeError(w, 404, "not_found")
			return
		}
		var id string
		if err = finalTx.QueryRow(r.Context(), `INSERT INTO attachments(chat_id,uploader_id,object_key,filename,content_type,size_bytes,preview_state,encrypted) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`, chatID, p.UserID, key, filename, contentType, size, state, encrypted).Scan(&id); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if state == "pending" && !encrypted {
			if _, err = finalTx.Exec(r.Context(), `INSERT INTO preview_jobs(attachment_id) VALUES($1)`, id); err != nil {
				writeError(w, 500, "internal")
				return
			}
		}
		if _, err = finalTx.Exec(r.Context(), `UPDATE upload_reservations SET state='complete' WHERE id=$1`, reservationID); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if err = finalTx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		committed = true
		writeJSON(w, 201, fileMeta{Encrypted: encrypted, ID: id, ChatID: chatID, Filename: filename, ContentType: contentType, Size: size, PreviewState: state, Available: true})
	})
	protected("PUT /api/v1/files/{id}/preview", func(w http.ResponseWriter, r *http.Request) { uploadEncryptedPreview(w, r, d) })
	protected("GET /api/v1/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		meta, err := authorizedFile(r, d, r.PathValue("id"), p.UserID)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		writeJSON(w, 200, meta)
	})
	for _, pattern := range []string{"GET /api/v1/files/{id}/content", "HEAD /api/v1/files/{id}/content", "GET /api/v1/files/{id}/preview", "HEAD /api/v1/files/{id}/preview"} {
		preview := strings.HasSuffix(pattern, "/preview")
		protected(pattern, func(w http.ResponseWriter, r *http.Request) { serveFile(w, r, d, preview) })
	}
}

func newObjectKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "files/" + hex.EncodeToString(b), nil
}

func authorizedFile(r *http.Request, d Dependencies, id, userID string) (fileMeta, error) {
	if !validUUID(id) {
		return fileMeta{}, errors.New("not found")
	}
	var m fileMeta
	err := d.DB.QueryRow(r.Context(), `SELECT a.id::text,a.chat_id::text,a.object_key,COALESCE(a.preview_key,''),a.filename,a.content_type,a.size_bytes,a.preview_state,a.encrypted
		FROM attachments a JOIN chats c ON c.id=a.chat_id JOIN chat_members cm ON cm.chat_id=c.id CROSS JOIN settings s
		WHERE a.id=$1 AND cm.user_id=$2 AND c.deleted_at IS NULL AND
		a.purged_at IS NULL AND (s.retention_days=0 OR a.created_at>now()-s.retention_days*interval '1 day') AND
		(EXISTS(SELECT 1 FROM message_attachments ma WHERE ma.attachment_id=a.id)
		 OR (a.uploader_id=$2 AND NOT EXISTS(SELECT 1 FROM message_attachments ma WHERE ma.attachment_id=a.id)))`, id, userID).Scan(&m.ID, &m.ChatID, &m.ObjectKey, &m.PreviewKey, &m.Filename, &m.ContentType, &m.Size, &m.PreviewState, &m.Encrypted)
	m.Available = err == nil
	return m, err
}

func parseByteRange(value string, size int64) (int64, int64, error) {
	match := rangePattern.FindStringSubmatch(value)
	if match == nil || (match[1] == "" && match[2] == "") || size <= 0 {
		return 0, 0, errors.New("invalid range")
	}
	if match[1] == "" {
		suffix, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, errors.New("invalid range")
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, nil
	}
	start, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || start >= size {
		return 0, 0, errors.New("invalid range")
	}
	end := size - 1
	if match[2] != "" {
		end, err = strconv.ParseInt(match[2], 10, 64)
		if err != nil || end < start {
			return 0, 0, errors.New("invalid range")
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, nil
}

func serveFile(w http.ResponseWriter, r *http.Request, d Dependencies, preview bool) {
	if d.Objects == nil {
		writeError(w, 503, "storage_unavailable")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	m, err := authorizedFile(r, d, r.PathValue("id"), p.UserID)
	if err != nil {
		writeError(w, 404, "not_found")
		return
	}
	key, size, ctype := m.ObjectKey, m.Size, m.ContentType
	if m.Encrypted {
		size = contentcrypto.StoredSize(size)
		ctype = "application/octet-stream"
	}
	if preview {
		if m.PreviewState != "ready" || m.PreviewKey == "" {
			writeError(w, 404, "not_found")
			return
		}
		key = m.PreviewKey
		meta, err := d.Objects.Head(r.Context(), key)
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		size = meta.Size
		ctype = meta.ContentType
	}
	if r.Method == http.MethodHead && !preview {
		if _, err := d.Objects.Head(r.Context(), key); err != nil {
			writeError(w, 404, "not_found")
			return
		}
	}
	contentRange := ""
	byteRange := ""
	status := 200
	if value := r.Header.Get("Range"); value != "" {
		start, end, err := parseByteRange(value, size)
		if err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			writeError(w, 416, "invalid_range")
			return
		}
		byteRange = fmt.Sprintf("bytes=%d-%d", start, end)
		contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, size)
		size = end - start + 1
		status = 206
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if contentRange != "" {
		w.Header().Set("Content-Range", contentRange)
	}
	disposition := "attachment"
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(ctype, ";", 2)[0]))
	inlineMedia := map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true, "image/avif": true, "video/mp4": true, "video/webm": true, "video/quicktime": true}
	if preview || inlineMedia[mediaType] {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": m.Filename}))
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	obj, err := d.Objects.Get(r.Context(), key, byteRange)
	if err != nil {
		w.Header().Del("Content-Length")
		writeError(w, 404, "not_found")
		return
	}
	defer obj.Body.Close()
	w.WriteHeader(status)
	_, _ = io.Copy(w, obj.Body)
}
