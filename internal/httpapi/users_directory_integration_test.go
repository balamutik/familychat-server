package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"familychat/server/internal/auth"
)

func TestUsersDirectoryPaginationAndSearchCompatibility(t *testing.T) {
	db := testDatabase(t)
	self, token := seedSession(t, db, "user")
	prefix := fmt.Sprintf("dir%x", time.Now().UnixNano())
	rows, err := db.Query(t.Context(), `INSERT INTO users(login,password_hash,display_name) SELECT $1||n::text,'test','Directory '||n::text FROM generate_series(1,105) n RETURNING id::text`, prefix)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(t.Context(), `DELETE FROM users WHERE id=ANY($1::uuid[])`, ids)
	if _, err = db.Exec(t.Context(), `UPDATE users SET disabled=true WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(t.Context(), `UPDATE users SET display_name='Contact name',avatar_key='avatars/directory-test' WHERE id=$1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{}
	rows, err = db.Query(t.Context(), `SELECT id::text FROM users WHERE NOT disabled`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		expected[id] = true
	}
	rows.Close()
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}})
	if r := callAPI(h, "GET", "/api/v1/users?directory=1", "", ""); r.Code != 401 {
		t.Fatalf("anonymous directory: %d", r.Code)
	}
	for _, suffix := range []string{"&after=bad", "&after=", "&limit=0", "&limit=101", "&limit=bad"} {
		r := callAPI(h, "GET", "/api/v1/users?directory=1"+suffix, "", token)
		if r.Code != 400 {
			t.Fatalf("invalid paging %s: %d %s", suffix, r.Code, r.Body.String())
		}
	}
	type directoryUser struct {
		ID          string `json:"id"`
		Login       string `json:"login"`
		DisplayName string `json:"display_name"`
		AvatarURL   string `json:"avatar_url"`
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	previous := ""
	for {
		path := "/api/v1/users?directory=1&limit=100"
		if cursor != "" {
			path += "&after=" + cursor
		}
		r := callAPI(h, "GET", path, "", token)
		var page struct {
			Users      []directoryUser `json:"users"`
			NextCursor *string         `json:"next_cursor"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &page) != nil {
			t.Fatalf("directory: %d %s", r.Code, r.Body.String())
		}
		if len(page.Users) > 100 {
			t.Fatalf("oversized page %d", len(page.Users))
		}
		pages++
		for _, u := range page.Users {
			if seen[u.ID] || u.ID <= previous || !expected[u.ID] {
				t.Fatalf("duplicate, unsorted or unexpected user: %+v", u)
			}
			seen[u.ID] = true
			previous = u.ID
			if u.ID == ids[1] && (u.DisplayName != "Contact name" || u.AvatarURL != avatarURL(u.ID)) {
				t.Fatalf("metadata lost: %+v", u)
			}
		}
		if page.NextCursor == nil {
			break
		}
		if len(page.Users) != 100 || *page.NextCursor != page.Users[len(page.Users)-1].ID || *page.NextCursor == cursor {
			t.Fatalf("bad cursor: %+v", page)
		}
		cursor = *page.NextCursor
	}
	if pages < 2 || len(seen) != len(expected) || !seen[self] || seen[ids[0]] {
		t.Fatalf("directory omitted users: pages=%d seen=%d expected=%d self=%v disabled=%v", pages, len(seen), len(expected), seen[self], seen[ids[0]])
	}
	// The original prefix-search endpoint still excludes self and caps at 20.
	r := callAPI(h, "GET", "/api/v1/users?q="+prefix, "", token)
	var search struct {
		Users []directoryUser `json:"users"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &search)
	if r.Code != 200 || len(search.Users) != 20 {
		t.Fatalf("search compatibility: %d %s", r.Code, r.Body.String())
	}
	var login string
	if err = db.QueryRow(t.Context(), `SELECT login FROM users WHERE id=$1`, self).Scan(&login); err != nil {
		t.Fatal(err)
	}
	r = callAPI(h, "GET", "/api/v1/users?q="+login, "", token)
	_ = json.Unmarshal(r.Body.Bytes(), &search)
	for _, u := range search.Users {
		if u.ID == self {
			t.Fatal("search included self")
		}
	}
	for _, path := range []string{"/api/v1/users", "/api/v1/users?q=a"} {
		r = callAPI(h, "GET", path, "", token)
		if r.Code != 400 {
			t.Fatalf("short search changed: %d", r.Code)
		}
	}
	// The directory needs no encryption negotiation because profiles are public to signed-in members.
	req := httptest.NewRequest("GET", "/api/v1/users?directory=1&limit=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("directory without content header: %d", w.Code)
	}
}
