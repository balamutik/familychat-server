package httpapi

import (
	"net/http"
	"strings"

	"familychat/server/internal/auth"
)

func registerUsers(mux *http.ServeMux, d Dependencies) {
	mux.Handle("GET /api/v1/users", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		if len(query) < 2 || len(query) > 32 {
			writeError(w, 400, "invalid_query")
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		rows, err := d.DB.Query(r.Context(), `SELECT id::text,login,avatar_key IS NOT NULL FROM users WHERE NOT disabled AND id<>$1 AND login LIKE $2||'%' ORDER BY login LIMIT 20`, p.UserID, query)
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		defer rows.Close()
		type user struct {
			ID        string `json:"id"`
			Login     string `json:"login"`
			AvatarURL string `json:"avatar_url,omitempty"`
		}
		users := []user{}
		for rows.Next() {
			var u user
			var hasAvatar bool
			if err := rows.Scan(&u.ID, &u.Login, &hasAvatar); err != nil {
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
