package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"familychat/server/internal/auth"
	"github.com/jackc/pgx/v5"
)

func adminOnly(d Dependencies, next http.HandlerFunc) http.Handler {
	return require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		if p.Role != "admin" {
			writeError(w, 403, "forbidden")
			return
		}
		next(w, r)
	}))
}

func registerAdmin(mux *http.ServeMux, d Dependencies) {
	mux.Handle("GET /api/v1/admin/settings/registration", adminOnly(d, func(w http.ResponseWriter, r *http.Request) {
		var enabled bool
		if err := d.DB.QueryRow(r.Context(), `SELECT registration_enabled FROM settings WHERE singleton=true`).Scan(&enabled); err != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 200, map[string]bool{"enabled": enabled})
	}))
	mux.Handle("PATCH /api/v1/admin/settings/registration", adminOnly(d, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Enabled == nil {
			writeError(w, 400, "invalid_setting")
			return
		}
		if _, err := d.DB.Exec(r.Context(), `UPDATE settings SET registration_enabled=$1 WHERE singleton=true`, *body.Enabled); err != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 200, map[string]bool{"enabled": *body.Enabled})
	}))
	mux.Handle("POST /api/v1/admin/users", adminOnly(d, func(w http.ResponseWriter, r *http.Request) {
		var c credentials
		if !decodeJSON(w, r, &c) {
			return
		}
		p, err := d.Auth.CreateUser(r.Context(), c.Login, c.Password)
		if errors.Is(err, auth.ErrConflict) {
			writeError(w, 409, "login_exists")
			return
		}
		if errors.Is(err, auth.ErrInvalidInput) {
			writeError(w, 400, "invalid_credentials")
			return
		}
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 201, p)
	}))
	mux.Handle("GET /api/v1/admin/users", adminOnly(d, func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		if len(cursor) > 100 {
			writeError(w, 400, "invalid_cursor")
			return
		}
		query := strings.ToLower(r.URL.Query().Get("q"))
		if len(query) > 32 {
			writeError(w, 400, "invalid_query")
			return
		}
		rows, err := d.DB.Query(r.Context(), `SELECT id::text,login,role,disabled,created_at FROM users
			WHERE login>$1 AND ($2='' OR login LIKE $2||'%') ORDER BY login LIMIT $3`, cursor, query, parseLimit(r)+1)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer rows.Close()
		type item struct {
			ID        string `json:"id"`
			Login     string `json:"login"`
			Role      string `json:"role"`
			Disabled  bool   `json:"disabled"`
			CreatedAt any    `json:"created_at"`
		}
		users := []item{}
		for rows.Next() {
			var v item
			if err := rows.Scan(&v.ID, &v.Login, &v.Role, &v.Disabled, &v.CreatedAt); err != nil {
				writeError(w, 500, "internal")
				return
			}
			users = append(users, v)
		}
		if rows.Err() != nil {
			writeError(w, 500, "internal")
			return
		}
		next := ""
		if len(users) > parseLimit(r) {
			next = users[parseLimit(r)-1].Login
			users = users[:parseLimit(r)]
		}
		writeJSON(w, 200, map[string]any{"users": users, "next_cursor": next})
	}))
	mux.Handle("PATCH /api/v1/admin/users/{id}", adminOnly(d, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Disabled *bool `json:"disabled"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Disabled == nil {
			writeError(w, 400, "invalid_setting")
			return
		}
		tx, err := d.DB.BeginTx(r.Context(), pgx.TxOptions{})
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		// Serialize all admin disabling decisions on the singleton settings row.
		if _, err = tx.Exec(r.Context(), `SELECT registration_enabled FROM settings WHERE singleton=true FOR UPDATE`); err != nil {
			writeError(w, 500, "internal")
			return
		}
		var role string
		id := r.PathValue("id")
		if err = tx.QueryRow(r.Context(), `SELECT role FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&role); err != nil {
			writeError(w, 404, "not_found")
			return
		}
		if *body.Disabled && role == "admin" {
			var remaining int
			if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM users WHERE role='admin' AND NOT disabled AND id<>$1`, id).Scan(&remaining); err != nil {
				writeError(w, 500, "internal")
				return
			}
			if remaining == 0 {
				writeError(w, 409, "last_admin")
				return
			}
		}
		if _, err = tx.Exec(r.Context(), `UPDATE users SET disabled=$2 WHERE id=$1`, id, *body.Disabled); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if *body.Disabled {
			if _, err = tx.Exec(r.Context(), `UPDATE sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, id); err != nil {
				writeError(w, 500, "internal")
				return
			}
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		if *body.Disabled && d.Events != nil {
			d.Events.CloseUser(id)
		}
		writeJSON(w, 200, map[string]any{"id": id, "disabled": *body.Disabled})
	}))
}
