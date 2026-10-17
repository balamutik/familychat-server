package httpapi

import (
	"net/http"
	"strconv"
)

// listUserDirectory uses a stable UUID keyset so a server's contacts can be
// loaded independently of the existing limited prefix-search endpoint.
func listUserDirectory(w http.ResponseWriter, r *http.Request, d Dependencies) {
	limit := 100
	query := r.URL.Query()
	if query.Has("limit") {
		var err error
		limit, err = strconv.Atoi(query.Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, 400, "invalid_limit")
			return
		}
	}
	after := query.Get("after")
	if query.Has("after") && !validUUID(after) {
		writeError(w, 400, "invalid_cursor")
		return
	}
	rows, err := d.DB.Query(r.Context(), `SELECT id::text,login,display_name,avatar_key IS NOT NULL FROM users
   WHERE NOT disabled AND ($1::uuid IS NULL OR id>$1::uuid) ORDER BY id ASC LIMIT $2`, nullableDirectoryCursor(after), limit+1)
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
		if err = rows.Scan(&u.ID, &u.Login, &u.DisplayName, &hasAvatar); err != nil {
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
	var nextCursor *string
	if len(users) > limit {
		users = users[:limit]
		cursor := users[len(users)-1].ID
		nextCursor = &cursor
	}
	writeJSON(w, 200, map[string]any{"users": users, "next_cursor": nextCursor})
}

func nullableDirectoryCursor(cursor string) any {
	if cursor == "" {
		return nil
	}
	return cursor
}
