package httpapi

import (
	"context"
	"encoding/json"
	"familychat/server/internal/auth"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDisplayNameRegistrationProfileAndChatFallback(t *testing.T) {
	db := testDatabase(t)
	ctx := context.Background()
	_, viewer := seedSession(t, db, "user")
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}})
	if _, err := db.Exec(ctx, `UPDATE settings SET registration_enabled=true`); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(ctx, `UPDATE settings SET registration_enabled=false`)
	login := fmt.Sprintf("name%d", time.Now().UnixNano())
	body := fmt.Sprintf(`{"login":%q,"password":"secure test password","display_name":"  Анна Петрова  "}`, login)
	r := callAPI(h, "POST", "/api/v1/auth/register", body, "")
	if r.Code != 201 {
		t.Fatalf("registration: %d %s", r.Code, r.Body)
	}
	var user struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	if user.DisplayName != "Анна Петрова" {
		t.Fatalf("name: %q", user.DisplayName)
	}
	defer db.Exec(ctx, `DELETE FROM users WHERE id=$1`, user.ID)
	r = callAPI(h, "POST", "/api/v1/auth/login", fmt.Sprintf(`{"login":%q,"password":"secure test password"}`, login), "")
	var session struct {
		Token string `json:"token"`
		User  struct {
			DisplayName string `json:"display_name"`
		} `json:"user"`
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &session) != nil || session.User.DisplayName != "Анна Петрова" {
		t.Fatalf("login: %s", r.Body)
	}
	r = callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, user.ID), viewer)
	var chat struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &chat) != nil || chat.Title != "Анна Петрова" {
		t.Fatalf("chat: %s", r.Body)
	}
	for _, query := range []string{"Анна", login} {
		r = callAPI(h, "GET", "/api/v1/users?q="+query, "", viewer)
		if r.Code != 200 || !strings.Contains(r.Body.String(), user.ID) {
			t.Fatalf("search: %s", r.Body)
		}
	}
	if r = callAPI(h, "PATCH", "/api/v1/users/me", `{"display_name":"Мама"}`, ""); r.Code != 401 {
		t.Fatalf("unauth: %d", r.Code)
	}
	for _, invalid := range []string{strings.Repeat("я", 65), "bad\nname"} {
		data, _ := json.Marshal(map[string]string{"display_name": invalid})
		if r = callAPI(h, "PATCH", "/api/v1/users/me", string(data), session.Token); r.Code != 400 {
			t.Fatalf("invalid: %d %s", r.Code, r.Body)
		}
	}
	r = callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/messages", `{"client_message_id":"00000000-0000-4000-8000-000000000001","text":"Hello"}`, session.Token)
	if r.Code != 201 {
		t.Fatalf("message: %d %s", r.Code, r.Body)
	}
	r = callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/calls", `{"kind":"audio"}`, session.Token)
	var call struct {
		ID string `json:"id"`
	}
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &call) != nil {
		t.Fatalf("call: %s", r.Body)
	}
	r = callAPI(h, "POST", "/api/v1/calls/"+call.ID+"/reject", "", viewer)
	if r.Code != 200 {
		t.Fatalf("reject: %d %s", r.Code, r.Body)
	}
	for _, name := range []string{"Мама", ""} {
		data, _ := json.Marshal(map[string]string{"display_name": name})
		if r = callAPI(h, "PATCH", "/api/v1/users/me", string(data), session.Token); r.Code != 200 {
			t.Fatalf("update: %d %s", r.Code, r.Body)
		}
		expected := name
		if expected == "" {
			expected = login
		}
		r = callAPI(h, "GET", "/api/v1/chats/"+chat.ID, "", viewer)
		if json.Unmarshal(r.Body.Bytes(), &chat) != nil || chat.Title != expected {
			t.Fatalf("title: %s", r.Body)
		}
		r = callAPI(h, "GET", "/api/v1/chats", "", viewer)
		var page struct {
			Chats []struct {
				Title  string `json:"title"`
				Sender string `json:"last_message_sender"`
			} `json:"chats"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &page) != nil || len(page.Chats) != 1 || page.Chats[0].Title != expected || page.Chats[0].Sender != expected {
			t.Fatalf("chat list: %s", r.Body)
		}
		r = callAPI(h, "GET", "/api/v1/calls", "", viewer)
		var history struct {
			Items []struct {
				Name string `json:"peer_name"`
			} `json:"items"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &history) != nil || len(history.Items) != 1 || history.Items[0].Name != expected {
			t.Fatalf("history: %s", r.Body)
		}
		r = callAPI(h, "GET", "/api/v1/chats/"+chat.ID+"/members", "", viewer)
		if !strings.Contains(r.Body.String(), login) || (name != "" && !strings.Contains(r.Body.String(), name)) {
			t.Fatalf("members: %s", r.Body)
		}
		r = callAPI(h, "GET", "/api/v1/auth/me", "", session.Token)
		var profile map[string]any
		if json.Unmarshal(r.Body.Bytes(), &profile) != nil || profile["login"] != login {
			t.Fatalf("profile: %s", r.Body)
		}
		if name != "" && profile["display_name"] != name {
			t.Fatalf("profile name: %s", r.Body)
		}
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM events WHERE chat_id=$1 AND kind='profile_updated'`, chat.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("profile events=%d: %v", count, err)
	}
}
