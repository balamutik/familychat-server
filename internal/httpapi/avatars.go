package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"familychat/server/internal/auth"
)

const maxAvatarBytes int64 = 5 << 20

func registerAvatars(mux *http.ServeMux, d Dependencies) {
	mux.Handle("PUT /api/v1/users/me/avatar", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.Objects == nil {
			writeError(w, 503, "storage_unavailable")
			return
		}
		contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
		if contentType != "image/jpeg" && contentType != "image/png" {
			writeError(w, 415, "unsupported_avatar_type")
			return
		}
		limit := maxAvatarBytes
		if hardLimit := maxAllowedFileBytes(d); hardLimit < limit {
			limit = hardLimit
		}
		var configuredLimit int64
		if err := d.DB.QueryRow(r.Context(), `SELECT max_file_bytes FROM settings WHERE singleton=true`).Scan(&configuredLimit); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if configuredLimit < limit {
			limit = configuredLimit
		}
		if r.ContentLength <= 0 || r.ContentLength > limit {
			writeError(w, 413, "invalid_file_size")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, 413, "invalid_file_size")
			} else {
				writeError(w, 400, "invalid_image")
			}
			return
		}
		if int64(len(data)) != r.ContentLength || !validAvatarImage(data, contentType) {
			writeError(w, 400, "invalid_image")
			return
		}
		quota := d.UserQuotaBytes
		if quota <= 0 {
			quota = 10 << 30
		}
		if int64(len(data)) > quota {
			writeError(w, 413, "quota_exceeded")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		key, err := newAvatarKey(p.UserID)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		committed := false
		defer func() {
			if !committed {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if d.Objects.Delete(ctx, key) != nil {
					_, _ = d.DB.Exec(ctx, `INSERT INTO avatar_gc(object_key) VALUES($1) ON CONFLICT DO NOTHING`, key)
				}
			}
		}()
		if err := d.Objects.Put(r.Context(), key, bytes.NewReader(data), int64(len(data)), contentType); err != nil {
			writeError(w, 502, "upload_failed")
			return
		}
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		var oldKey string
		if err := tx.QueryRow(r.Context(), `SELECT COALESCE(avatar_key,'') FROM users WHERE id=$1 AND NOT disabled FOR UPDATE`, p.UserID).Scan(&oldKey); err != nil {
			writeError(w, 401, "unauthorized")
			return
		}
		var usedOther int64
		if err := tx.QueryRow(r.Context(), `SELECT COALESCE((SELECT sum(size_bytes) FROM attachments WHERE uploader_id=$1 AND purged_at IS NULL),0)
			+COALESCE((SELECT sum(reserved_bytes) FROM upload_reservations WHERE user_id=$1 AND state='uploading' AND expires_at>now()),0)`, p.UserID).Scan(&usedOther); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if usedOther > quota-int64(len(data)) {
			writeError(w, 413, "quota_exceeded")
			return
		}
		if _, err := tx.Exec(r.Context(), `UPDATE users SET avatar_key=$2,avatar_content_type=$3,avatar_size_bytes=$4 WHERE id=$1`, p.UserID, key, contentType, len(data)); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if oldKey != "" {
			if _, err := tx.Exec(r.Context(), `INSERT INTO avatar_gc(object_key) VALUES($1) ON CONFLICT DO NOTHING`, oldKey); err != nil {
				writeError(w, 500, "internal")
				return
			}
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		committed = true
		writeJSON(w, 200, map[string]string{"avatar_url": avatarURL(p.UserID)})
	})))

	mux.Handle("DELETE /api/v1/users/me/avatar", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		var key string
		if err := tx.QueryRow(r.Context(), `SELECT COALESCE(avatar_key,'') FROM users WHERE id=$1 AND NOT disabled FOR UPDATE`, p.UserID).Scan(&key); err != nil {
			writeError(w, 401, "unauthorized")
			return
		}
		if key != "" {
			if _, err := tx.Exec(r.Context(), `UPDATE users SET avatar_key=NULL,avatar_content_type=NULL,avatar_size_bytes=NULL WHERE id=$1`, p.UserID); err != nil {
				writeError(w, 500, "internal")
				return
			}
			if _, err := tx.Exec(r.Context(), `INSERT INTO avatar_gc(object_key) VALUES($1) ON CONFLICT DO NOTHING`, key); err != nil {
				writeError(w, 500, "internal")
				return
			}
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		w.WriteHeader(204)
	})))

	for _, pattern := range []string{"GET /api/v1/users/{userID}/avatar", "HEAD /api/v1/users/{userID}/avatar"} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
			if d.Objects == nil {
				writeError(w, 503, "storage_unavailable")
				return
			}
			id := r.PathValue("userID")
			if !validUUID(id) {
				writeError(w, 404, "not_found")
				return
			}
			var key, contentType string
			var size int64
			if err := d.DB.QueryRow(r.Context(), `SELECT avatar_key,avatar_content_type,avatar_size_bytes FROM users WHERE id=$1 AND NOT disabled AND avatar_key IS NOT NULL`, id).Scan(&key, &contentType, &size); err != nil {
				writeError(w, 404, "not_found")
				return
			}
			var body io.ReadCloser
			if r.Method == http.MethodHead {
				if _, err := d.Objects.Head(r.Context(), key); err != nil {
					writeError(w, 404, "not_found")
					return
				}
			} else {
				obj, err := d.Objects.Get(r.Context(), key, "")
				if err != nil {
					writeError(w, 404, "not_found")
					return
				}
				body = obj.Body
				defer body.Close()
			}
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.Header().Set("Content-Disposition", "inline")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(200)
			if body != nil {
				_, _ = io.Copy(w, body)
			}
		})
	}
}

func validAvatarImage(data []byte, contentType string) bool {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 || int64(config.Width)*int64(config.Height) > 16_777_216 {
		return false
	}
	if (contentType == "image/jpeg" && format != "jpeg") || (contentType == "image/png" && format != "png") {
		return false
	}
	_, decodedFormat, err := image.Decode(bytes.NewReader(data))
	return err == nil && decodedFormat == format
}

func newAvatarKey(userID string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "avatars/" + userID + "/" + hex.EncodeToString(b[:]), nil
}

func avatarURL(userID string) string { return "/api/v1/users/" + userID + "/avatar" }
