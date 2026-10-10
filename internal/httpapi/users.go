package httpapi

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"familychat/server/internal/auth"
)

func registerUsers(mux *http.ServeMux, d Dependencies) {
	mux.Handle("PATCH /api/v1/users/me", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DisplayName *string `json:"display_name"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.DisplayName == nil {
			writeError(w, 400, "invalid_display_name")
			return
		}
		name, err := auth.NormalizeDisplayName(*body.DisplayName)
		if err != nil {
			writeError(w, 400, "invalid_display_name")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		tx, err := d.DB.Begin(r.Context())
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer tx.Rollback(r.Context())
		result, err := tx.Exec(r.Context(), `UPDATE users SET display_name=$2 WHERE id=$1 AND display_name<>$2`, p.UserID, name)
		if err == nil && result.RowsAffected() > 0 {
			_, err = tx.Exec(r.Context(), `INSERT INTO events(chat_id,kind,payload)
    SELECT cm.chat_id,'profile_updated',jsonb_build_object('user_id',$1::text)
    FROM chat_members cm JOIN chats c ON c.id=cm.chat_id WHERE cm.user_id=$1::uuid AND c.deleted_at IS NULL`, p.UserID)
		}
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeError(w, 500, "internal")
			return
		}
		p.DisplayName = name
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, p)
	})))
	mux.Handle("GET /api/v1/users", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		if utf8.RuneCountInString(query) < 2 || utf8.RuneCountInString(query) > 64 {
			writeError(w, 400, "invalid_query")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		rows, err := d.DB.Query(r.Context(), `SELECT id::text,login,display_name,avatar_key IS NOT NULL FROM users WHERE NOT disabled AND id<>$1 AND (starts_with(login,$2) OR starts_with(lower(display_name),$2)) ORDER BY COALESCE(NULLIF(display_name,''),login),login LIMIT 20`, p.UserID, query)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer rows.Close()
		type user struct {
			DisplayName string `json:"display_name,omitempty"`
			ID          string `json:"id"`
			Login       string `json:"login"`
			AvatarURL   string `json:"avatar_url,omitempty"`
		}
		users := []user{}
		for rows.Next() {
			var u user
			var hasAvatar bool
			if err := rows.Scan(&u.ID, &u.Login, &u.DisplayName, &hasAvatar); err != nil {
				writeError(w, 500, "internal")
				return
			}
			if hasAvatar {
				u.AvatarURL = avatarURL(u.ID)
			}
			users = append(users, u)
		}
		if rows.Err() != nil {
			writeError(w, 500, "internal")
			return
		}
		writeJSON(w, 200, map[string]any{"users": users})
	})))
}
