package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"familychat/server/internal/auth"
	"familychat/server/internal/contentcrypto"
	"github.com/jackc/pgx/v5"
)

type storyView struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Login       string    `json:"login"`
	DisplayName string    `json:"display_name,omitempty"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
	Caption     string    `json:"caption"`
	ContentType string    `json:"content_type"`
	Filename    string    `json:"filename"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Viewed      bool      `json:"viewed"`
	Encrypted   bool      `json:"encrypted"`
}

const storyColumns = `s.id::text,s.user_id::text,u.login,u.display_name,CASE WHEN u.avatar_key IS NOT NULL THEN '/api/v1/users/'||u.id::text||'/avatar' ELSE '' END,s.caption,s.content_type,s.filename,s.size_bytes,s.created_at,s.expires_at,EXISTS(SELECT 1 FROM story_views sv WHERE sv.story_id=s.id AND sv.user_id=$1),s.encrypted`
const storyVisible = `s.state='ready' AND s.deleted_at IS NULL AND s.purged_at IS NULL AND s.expires_at>now() AND NOT u.disabled AND NOT EXISTS(SELECT 1 FROM user_blocks b WHERE (b.source_user_id=$1 AND b.target_user_id=s.user_id) OR (b.source_user_id=s.user_id AND b.target_user_id=$1))`

func storyVisibility(d Dependencies) string {
	if d.ContentKey != nil {
		return storyVisible + " AND s.encrypted"
	}
	return storyVisible
}

func scanStory(row pgx.Row) (storyView, error) {
	var v storyView
	err := row.Scan(&v.ID, &v.UserID, &v.Login, &v.DisplayName, &v.AvatarURL, &v.Caption, &v.ContentType, &v.Filename, &v.Size, &v.CreatedAt, &v.ExpiresAt, &v.Viewed, &v.Encrypted)
	return v, err
}

func registerStories(mux *http.ServeMux, d Dependencies) {
	protected := func(pattern string, f http.HandlerFunc) { mux.Handle(pattern, require(d.Auth, f)) }
	protected("GET /api/v1/stories", func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		rows, err := d.DB.Query(r.Context(), `SELECT `+storyColumns+` FROM stories s JOIN users u ON u.id=s.user_id WHERE `+storyVisibility(d)+` ORDER BY s.created_at DESC,s.id DESC`, p.UserID)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer rows.Close()
		list := []storyView{}
		for rows.Next() {
			v, e := scanStory(rows)
			if e != nil {
				writeError(w, 500, "internal")
				return
			}
			list = append(list, v)
		}
		if rows.Err() != nil {
			writeError(w, 500, "internal")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"stories": list})
	})
	protected("POST /api/v1/stories", func(w http.ResponseWriter, r *http.Request) { uploadStory(w, r, d) })
	protected("GET /api/v1/stories/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validUUID(id) {
			writeError(w, 404, "not_found")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		v, err := scanStory(d.DB.QueryRow(r.Context(), `SELECT `+storyColumns+` FROM stories s JOIN users u ON u.id=s.user_id WHERE `+storyVisibility(d)+` AND s.id=$2`, p.UserID, id))
		if err != nil {
			writeError(w, 404, "not_found")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, v)
	})
	protected("PUT /api/v1/stories/{id}/view", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validUUID(id) {
			writeError(w, 404, "not_found")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		result, err := d.DB.Exec(r.Context(), `INSERT INTO story_views(story_id,user_id) SELECT s.id,$1 FROM stories s JOIN users u ON u.id=s.user_id WHERE s.id=$2 AND `+storyVisibility(d)+` ON CONFLICT(story_id,user_id) DO UPDATE SET viewed_at=story_views.viewed_at`, p.UserID, id)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		if result.RowsAffected() == 0 {
			writeError(w, 404, "not_found")
			return
		}
		w.WriteHeader(204)
	})
	protected("DELETE /api/v1/stories/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validUUID(id) {
			writeError(w, 404, "not_found")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		result, err := d.DB.Exec(r.Context(), `UPDATE stories SET deleted_at=COALESCE(deleted_at,now()) WHERE id=$1 AND user_id=$2`, id, p.UserID)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		if result.RowsAffected() == 0 {
			writeError(w, 404, "not_found")
			return
		}
		w.WriteHeader(204)
	})
	for _, method := range []string{"GET", "HEAD"} {
		protected(method+" /api/v1/stories/{id}/content", func(w http.ResponseWriter, r *http.Request) { serveStory(w, r, d) })
	}
}

func uploadStory(w http.ResponseWriter, r *http.Request, d Dependencies) {
	if d.Objects == nil {
		writeError(w, 503, "storage_unavailable")
		return
	}
	encrypted := r.Header.Get("X-Content-Encryption") == contentcrypto.Scheme
	if d.ContentKey != nil && !encrypted {
		writeError(w, 426, "encryption_required")
		return
	}
	if encrypted && d.ContentKey == nil {
		writeError(w, 400, "invalid_encrypted_content")
		return
	}
	ctype := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	allowed := map[string]bool{"image/jpeg": true, "image/png": true, "image/heic": true, "image/heif": true, "image/webp": true, "image/gif": true, "video/mp4": true, "video/quicktime": true, "video/webm": true}
	if !allowed[ctype] {
		writeError(w, 400, "invalid_media_type")
		return
	}
	filename, err := uploadFilename(r.Header)
	if err != nil {
		writeError(w, 400, "invalid_filename")
		return
	}
	caption, err := url.PathUnescape(r.Header.Get("X-Story-Caption"))
	if err != nil || !utf8.ValidString(caption) {
		writeError(w, 400, "invalid_caption")
		return
	}
	if encrypted {
		if caption != "" && (len(caption) > 5600 || d.ContentKey.ValidateText(caption) != nil) {
			writeError(w, 400, "invalid_encrypted_content")
			return
		}
	} else if utf8.RuneCountInString(caption) > 1000 {
		writeError(w, 400, "invalid_caption")
		return
	}
	size, storedSize := r.ContentLength, r.ContentLength
	if encrypted {
		size, err = strconv.ParseInt(r.Header.Get("X-Plaintext-Size"), 10, 64)
		if err != nil || size <= 0 || contentcrypto.StoredSize(size) != storedSize {
			writeError(w, 400, "invalid_encrypted_content")
			return
		}
	}
	var configuredLimit int64
	if err = d.DB.QueryRow(r.Context(), `SELECT max_file_bytes FROM settings WHERE singleton=true`).Scan(&configuredLimit); err != nil {
		writeError(w, 500, "internal")
		return
	}
	limit := maxAllowedFileBytes(d)
	if limit > 50<<20 {
		limit = 50 << 20
	}
	if configuredLimit < limit {
		limit = configuredLimit
	}
	if size <= 0 || size > limit {
		writeError(w, 413, "invalid_file_size")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	key, err := newObjectKey()
	if err != nil {
		writeError(w, 500, "internal")
		return
	}
	key = "stories/" + strings.TrimPrefix(key, "files/")
	tx, err := d.DB.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "internal")
		return
	}
	defer tx.Rollback(r.Context())
	var user string
	if err = tx.QueryRow(r.Context(), `SELECT id::text FROM users WHERE id=$1 AND NOT disabled FOR UPDATE`, p.UserID).Scan(&user); err != nil {
		writeError(w, 404, "not_found")
		return
	}
	quota := d.UserQuotaBytes
	if quota <= 0 {
		quota = 10 << 30
	}
	var used int64
	err = tx.QueryRow(r.Context(), `SELECT COALESCE((SELECT sum(size_bytes+preview_size_bytes+CASE WHEN encrypted THEN 80+16*((size_bytes+1048575)/1048576) ELSE 0 END) FROM attachments WHERE uploader_id=$1 AND purged_at IS NULL),0)
 +COALESCE((SELECT avatar_size_bytes FROM users WHERE id=$1),0)
 +COALESCE((SELECT sum(reserved_bytes) FROM upload_reservations WHERE user_id=$1 AND state='uploading' AND expires_at>now()),0)
 +COALESCE((SELECT sum(stored_size_bytes) FROM stories WHERE user_id=$1 AND purged_at IS NULL),0)`, p.UserID).Scan(&used)
	if err != nil {
		writeError(w, 500, "internal")
		return
	}
	if used > quota-storedSize {
		writeError(w, 413, "quota_exceeded")
		return
	}
	var id string
	err = tx.QueryRow(r.Context(), `INSERT INTO stories(user_id,object_key,filename,content_type,size_bytes,stored_size_bytes,encrypted,caption) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`, p.UserID, key, filename, ctype, size, storedSize, encrypted, caption).Scan(&id)
	if err != nil {
		writeError(w, 500, "internal")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "internal")
		return
	}
	committed := false
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = d.DB.Exec(r.Context(), `UPDATE stories SET upload_deadline=now()+interval '30 minutes' WHERE id=$1 AND state='uploading' AND deleted_at IS NULL AND purged_at IS NULL`, id)
			}
		}
	}()
	defer func() {
		if !committed {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			deletionErr := d.Objects.Delete(ctx, key)
			_, _ = d.DB.Exec(ctx, `UPDATE stories SET state='failed',deleted_at=now(),purged_at=CASE WHEN $2 THEN now() ELSE NULL END WHERE id=$1`, id, deletionErr == nil)
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, storedSize)
	var body io.Reader = r.Body
	objectType := ctype
	if encrypted {
		header := make([]byte, contentcrypto.HeaderSize)
		if _, err = io.ReadFull(r.Body, header); err != nil {
			writeError(w, 400, "invalid_encrypted_content")
			return
		}
		n, e := d.ContentKey.HeaderSize(header)
		if e != nil || n != size {
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
	result, err := d.DB.Exec(r.Context(), `UPDATE stories SET state='ready',created_at=now(),expires_at=now()+interval '24 hours' WHERE id=$1 AND state='uploading' AND deleted_at IS NULL AND purged_at IS NULL AND upload_deadline>now() AND EXISTS(SELECT 1 FROM users WHERE id=$2 AND NOT disabled)`, id, p.UserID)
	if err != nil {
		writeError(w, 500, "internal")
		return
	}
	if result.RowsAffected() == 0 {
		writeError(w, 409, "upload_expired")
		return
	}
	committed = true
	v, err := scanStory(d.DB.QueryRow(r.Context(), `SELECT `+storyColumns+` FROM stories s JOIN users u ON u.id=s.user_id WHERE s.id=$2`, p.UserID, id))
	if err != nil {
		writeError(w, 500, "internal")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 201, v)
}

func serveStory(w http.ResponseWriter, r *http.Request, d Dependencies) {
	if d.Objects == nil {
		writeError(w, 503, "storage_unavailable")
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
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
	// A row read lease lasts until streaming completes. Expiry cleanup skips active readers.
	var key, filename, ctype string
	var size int64
	var encrypted bool
	err = tx.QueryRow(r.Context(), `SELECT s.object_key,s.filename,s.content_type,s.stored_size_bytes,s.encrypted FROM stories s JOIN users u ON u.id=s.user_id WHERE s.id=$2 AND `+storyVisibility(d)+` FOR SHARE OF s`, p.UserID, id).Scan(&key, &filename, &ctype, &size, &encrypted)
	if err != nil {
		writeError(w, 404, "not_found")
		return
	}
	if encrypted {
		ctype = "application/octet-stream"
	}
	status := 200
	byteRange := ""
	if value := r.Header.Get("Range"); value != "" {
		start, end, e := parseByteRange(value, size)
		if e != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			writeError(w, 416, "invalid_range")
			return
		}
		byteRange = fmt.Sprintf("bytes=%d-%d", start, end)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		size = end - start + 1
		status = 206
	}
	if r.Method == http.MethodHead {
		if _, err = d.Objects.Head(r.Context(), key); err != nil {
			writeError(w, 404, "not_found")
			return
		}
	}
	var body io.ReadCloser
	if r.Method != http.MethodHead {
		obj, e := d.Objects.Get(r.Context(), key, byteRange)
		if e != nil {
			writeError(w, 404, "not_found")
			return
		}
		body = obj.Body
		defer body.Close()
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": filename}))
	w.WriteHeader(status)
	if body != nil {
		_, _ = io.Copy(w, body)
	}
}
