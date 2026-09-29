package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"familychat/server/internal/auth"
)

var rangePattern = regexp.MustCompile(`^bytes=(\d*)-(\d*)$`)

type fileMeta struct {
	ID           string `json:"id"`
	ChatID       string `json:"chat_id"`
	Filename     string `json:"filename"`
	ContentType  string `json:"content_type"`
	Size         int64  `json:"size"`
	PreviewState string `json:"preview_state"`
	ObjectKey    string `json:"-"`
	PreviewKey   string `json:"-"`
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
		size := r.ContentLength
		limit := d.MaxFileBytes
		if limit <= 0 {
			limit = 250 << 20
		}
		quota := d.UserQuotaBytes
		if quota <= 0 {
			quota = 10 << 30
		}
		if size <= 0 || size > limit {
			writeError(w, 413, "invalid_file_size")
			return
		}
		filename := filepath.Base(strings.TrimSpace(r.Header.Get("X-File-Name")))
		if filename == "" || filename == "." || filename == "/" || len(filename) > 255 || strings.ContainsAny(filename, "\x00\r\n\\") {
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
		if err = tx.QueryRow(r.Context(), `SELECT COALESCE((SELECT sum(size_bytes) FROM attachments WHERE uploader_id=$1),0)+COALESCE((SELECT sum(reserved_bytes) FROM upload_reservations WHERE user_id=$1 AND state='uploading' AND expires_at>now()),0)`, p.UserID).Scan(&used); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if used > quota-size {
			writeError(w, 413, "quota_exceeded")
			return
		}
		var reservationID string
		if err = tx.QueryRow(r.Context(), `INSERT INTO upload_reservations(user_id,chat_id,object_key,reserved_bytes,state,expires_at) VALUES($1,$2,$3,$4,'uploading',now()+interval '30 minutes') RETURNING id::text`, p.UserID, chatID, key, size).Scan(&reservationID); err != nil {
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
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if err = d.Objects.Put(r.Context(), key, r.Body, size, contentType); err != nil {
			writeError(w, 502, "upload_failed")
			return
		}
		meta, err := d.Objects.Head(r.Context(), key)
		if err != nil || meta.Size != size {
			writeError(w, 502, "upload_incomplete")
			return
		}
		state := "unsupported"
		if strings.HasPrefix(contentType, "image/") || strings.HasPrefix(contentType, "video/") {
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
		if err = finalTx.QueryRow(r.Context(), `INSERT INTO attachments(chat_id,uploader_id,object_key,filename,content_type,size_bytes,preview_state) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id::text`, chatID, p.UserID, key, filename, contentType, size, state).Scan(&id); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if state == "pending" {
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
		writeJSON(w, 201, fileMeta{ID: id, ChatID: chatID, Filename: filename, ContentType: contentType, Size: size, PreviewState: state})
	})
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
	err := d.DB.QueryRow(r.Context(), `SELECT a.id::text,a.chat_id::text,a.object_key,COALESCE(a.preview_key,''),a.filename,a.content_type,a.size_bytes,a.preview_state
		FROM attachments a JOIN chats c ON c.id=a.chat_id JOIN chat_members cm ON cm.chat_id=c.id
		WHERE a.id=$1 AND cm.user_id=$2 AND c.deleted_at IS NULL AND
		(EXISTS(SELECT 1 FROM message_attachments ma WHERE ma.attachment_id=a.id)
		 OR (a.uploader_id=$2 AND NOT EXISTS(SELECT 1 FROM message_attachments ma WHERE ma.attachment_id=a.id)))`, id, userID).Scan(&m.ID, &m.ChatID, &m.ObjectKey, &m.PreviewKey, &m.Filename, &m.ContentType, &m.Size, &m.PreviewState)
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
