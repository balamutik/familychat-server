package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"familychat/server/internal/auth"
)

type reactionResponse struct {
	messageView
	Reactions []struct {
		Emoji   string   `json:"emoji"`
		UserIDs []string `json:"user_ids"`
	} `json:"reactions"`
}

func reactionFixture(t *testing.T) (http.Handler, string, string, string, string, string) {
	t.Helper()
	db := testDatabase(t)
	aid, a := seedSession(t, db, "user")
	bid, b := seedSession(t, db, "user")
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}, PushEnabled: true})
	r := callAPI(h, "POST", "/api/v1/chats/direct", fmt.Sprintf(`{"user_id":%q}`, bid), a)
	var chat chatView
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &chat) != nil {
		t.Fatalf("chat: %d %s", r.Code, r.Body.String())
	}
	r = callAPI(h, "POST", "/api/v1/chats/"+chat.ID+"/messages", `{"client_message_id":"00000000-0000-4000-8000-000000001701","text":"reaction target"}`, a)
	var message messageView
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &message) != nil {
		t.Fatalf("message: %d %s", r.Code, r.Body.String())
	}
	if !strings.Contains(r.Body.String(), `"reactions":[]`) {
		t.Fatalf("new message reactions: %s", r.Body.String())
	}
	return h, "/api/v1/chats/" + chat.ID + "/messages/" + message.ID, aid, a, bid, b
}

func TestMessageReactionsPersistAggregateReplaceAndRemoveIdempotently(t *testing.T) {
	h, path, aid, a, bid, b := reactionFixture(t)
	db := testDatabase(t)
	parts := strings.Split(path, "/")
	chatID, messageID := parts[4], parts[6]
	if r := callAPI(h, "PUT", "/api/v1/push/devices/00000000-0000-4000-8000-000000001702", fmt.Sprintf(`{"client_server_id":"00000000-0000-4000-8000-000000001703","alert_token":%q}`, strings.Repeat("a", 64)), b); r.Code != 204 {
		t.Fatalf("push registration: %d %s", r.Code, r.Body.String())
	}
	set := func(token, emoji string) reactionResponse {
		t.Helper()
		r := callAPI(h, "PUT", path+"/reaction", fmt.Sprintf(`{"emoji":%q}`, emoji), token)
		var v reactionResponse
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &v) != nil {
			t.Fatalf("set: %d %s", r.Code, r.Body.String())
		}
		if v.ID != messageID || v.Seq != 1 || v.Text != "reaction target" {
			t.Fatalf("message identity changed: %+v", v)
		}
		return v
	}
	set(a, "👍") // Reactions also work on one's own message.
	set(b, "👍")
	v := set(b, "👍")
	ids := []string{aid, bid}
	sort.Strings(ids)
	if len(v.Reactions) != 1 || v.Reactions[0].Emoji != "👍" || !reflect.DeepEqual(v.Reactions[0].UserIDs, ids) {
		t.Fatalf("aggregate: %+v", v.Reactions)
	}
	v = set(b, "❤️")
	if len(v.Reactions) != 2 || v.Reactions[0].Emoji != "❤️" || !reflect.DeepEqual(v.Reactions[0].UserIDs, []string{bid}) || v.Reactions[1].Emoji != "👍" || !reflect.DeepEqual(v.Reactions[1].UserIDs, []string{aid}) {
		t.Fatalf("replace/sort: %+v", v.Reactions)
	}
	for _, suffix := range []string{"", "/messages", "/messages?after=0", "/messages?ids=" + messageID, "/search?q=reaction"} {
		url := "/api/v1/chats/" + chatID + suffix
		if suffix == "" {
			url = path
		}
		r := callAPI(h, "GET", url, "", b)
		if r.Code != 200 || !strings.Contains(r.Body.String(), `"reactions":[{"emoji":"❤️","user_ids":["`+bid+`"]},{"emoji":"👍","user_ids":["`+aid+`"]}]`) {
			t.Fatalf("metadata %s: %d %s", url, r.Code, r.Body.String())
		}
	}
	r := callAPI(h, "PATCH", path, `{"text":"edited target"}`, a)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"emoji":"❤️"`) {
		t.Fatalf("edit metadata: %d %s", r.Code, r.Body.String())
	}
	for _, token := range []string{b, b, a, a} {
		r = callAPI(h, "DELETE", path+"/reaction", "", token)
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &v) != nil {
			t.Fatalf("remove: %d %s", r.Code, r.Body.String())
		}
	}
	if v.Reactions == nil || len(v.Reactions) != 0 {
		t.Fatalf("empty reactions must be []: %s", r.Body.String())
	}
	var updates, reactions, pushes int
	var nextSeq int64
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE chat_id=$1 AND kind='message_updated' AND payload->>'message_id'=$2 AND (payload->>'seq')::bigint=1`, chatID, messageID).Scan(&updates); err != nil || updates != 6 {
		t.Fatalf("change events: %d %v", updates, err)
	}
	if err := db.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM message_reactions WHERE message_id=$1),(SELECT next_seq FROM chats WHERE id=$2),(SELECT count(*) FROM push_jobs j JOIN push_devices d ON d.id=j.device_id WHERE d.user_id=$3)`, messageID, chatID, bid).Scan(&reactions, &nextSeq, &pushes); err != nil || reactions != 0 || nextSeq != 2 || pushes != 0 {
		t.Fatalf("side effects: reactions=%d next=%d pushes=%d err=%v", reactions, nextSeq, pushes, err)
	}
}

func TestMessageReactionsEnforceVisibilityAndBlocking(t *testing.T) {
	h, path, _, a, bid, b := reactionFixture(t)
	db := testDatabase(t)
	_, outsider := seedSession(t, db, "user")
	for _, method := range []string{"PUT", "DELETE"} {
		for _, tc := range []struct {
			token, path string
			status      int
		}{{"", path, 401}, {outsider, path, 404}, {a, path + "bad", 404}, {a, strings.Replace(path, strings.Split(path, "/")[4], "00000000-0000-4000-8000-000000001799", 1), 404}} {
			if r := callAPI(h, method, tc.path+"/reaction", `{"emoji":"👍"}`, tc.token); r.Code != tc.status {
				t.Fatalf("access %s: %d %s", method, r.Code, r.Body.String())
			}
		}
	}
	if r := callAPI(h, "PUT", path+"/reaction", `{"emoji":"👍"}`, b); r.Code != 200 {
		t.Fatalf("initial: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "PUT", "/api/v1/users/"+bid+"/block", `{"blocked":true}`, a); r.Code != 204 {
		t.Fatalf("block: %d", r.Code)
	}
	for _, token := range []string{a, b} {
		if r := callAPI(h, "PUT", path+"/reaction", `{"emoji":"❤️"}`, token); r.Code != 403 {
			t.Fatalf("blocked set: %d %s", r.Code, r.Body.String())
		}
	}
	if r := callAPI(h, "DELETE", path+"/reaction", "", b); r.Code != 200 || !strings.Contains(r.Body.String(), `"reactions":[]`) {
		t.Fatalf("blocked removal: %d %s", r.Code, r.Body.String())
	}
	if r := callAPI(h, "DELETE", path+"?scope=self", "", b); r.Code != 204 {
		t.Fatalf("hide: %d", r.Code)
	}
	for _, method := range []string{"PUT", "DELETE"} {
		if r := callAPI(h, method, path+"/reaction", `{"emoji":"👍"}`, b); r.Code != 404 {
			t.Fatalf("hidden %s: %d", method, r.Code)
		}
	}
	callAPI(h, "DELETE", path+"?scope=everyone", "", a)
	for _, method := range []string{"PUT", "DELETE"} {
		if r := callAPI(h, method, path+"/reaction", `{"emoji":"👍"}`, a); r.Code != 404 {
			t.Fatalf("deleted %s: %d", method, r.Code)
		}
	}
}

func TestMessageReactionAcceptsOneEmojiGrapheme(t *testing.T) {
	h, path, _, a, _, _ := reactionFixture(t)
	for _, emoji := range []string{"👍", "👍🏽", "❤️", "☺", "👨‍👩‍👧‍👦", "🏳️‍🌈", "🇷🇺", "1️⃣", "#️⃣", "🫨", "🏴\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f"} {
		body, _ := json.Marshal(map[string]string{"emoji": emoji})
		if r := callAPI(h, "PUT", path+"/reaction", string(body), a); r.Code != 200 {
			t.Errorf("valid %q: %d %s", emoji, r.Code, r.Body.String())
		}
	}
	for _, emoji := range []string{"", "text", "a", "1", "#", " ", "👍 👍", "👍👍", "👍\n", "👍a", "a\u0301", "🏽", "🇷", "\u200d", "\ufe0f", "💖\u200d", "\u200d💖", "👍\u0000", strings.Repeat("👍\u200d", 100) + "👍"} {
		body, _ := json.Marshal(map[string]string{"emoji": emoji})
		if r := callAPI(h, "PUT", path+"/reaction", string(body), a); r.Code != 400 || !strings.Contains(r.Body.String(), "invalid_reaction") {
			t.Errorf("invalid %q: %d %s", emoji, r.Code, r.Body.String())
		}
	}
}

func TestMessageReactionConcurrencyDoesNotUpdateDeletedMessages(t *testing.T) {
	h, path, _, a, bid, b := reactionFixture(t)
	db := testDatabase(t)
	messageID := strings.Split(path, "/")[6]
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := callAPI(h, "PUT", path+"/reaction", `{"emoji":"👍"}`, b)
			if r.Code != 200 && r.Code != 404 {
				t.Errorf("concurrent set: %d %s", r.Code, r.Body.String())
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if r := callAPI(h, "DELETE", path+"?scope=everyone", "", a); r.Code != 204 {
			t.Errorf("delete: %d", r.Code)
		}
	}()
	wg.Wait()
	var rows, lateUpdates int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM message_reactions WHERE message_id=$1 AND user_id=$2`, messageID, bid).Scan(&rows); err != nil || rows > 1 {
		t.Fatalf("unique caller reaction: %d %v", rows, err)
	}
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM events e WHERE e.kind='message_updated' AND e.payload->>'message_id'=$1 AND e.id>(SELECT id FROM events WHERE kind='message_deleted' AND payload->>'message_id'=$1)`, messageID).Scan(&lateUpdates); err != nil || lateUpdates != 0 {
		t.Fatalf("updates after deletion: %d %v", lateUpdates, err)
	}
}

func TestMessageReactionGroupMembershipAndCascadingDeletion(t *testing.T) {
	db := testDatabase(t)
	_, owner := seedSession(t, db, "user")
	memberID, member := seedSession(t, db, "user")
	h := New(Dependencies{DB: db, Auth: &auth.Service{DB: db, TTL: time.Hour}})
	r := callAPI(h, "POST", "/api/v1/chats", `{"title":"reaction group"}`, owner)
	var chat chatView
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &chat) != nil {
		t.Fatalf("group: %d %s", r.Code, r.Body.String())
	}
	base := "/api/v1/chats/" + chat.ID
	if r = callAPI(h, "POST", base+"/members", fmt.Sprintf(`{"user_id":%q}`, memberID), owner); r.Code != 201 {
		t.Fatalf("add member: %d %s", r.Code, r.Body.String())
	}
	r = callAPI(h, "POST", base+"/messages", `{"client_message_id":"00000000-0000-4000-8000-000000001704","text":"group target"}`, owner)
	var message messageView
	if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), &message) != nil {
		t.Fatalf("group message: %d %s", r.Code, r.Body.String())
	}
	path := base + "/messages/" + message.ID + "/reaction"
	if r = callAPI(h, "PUT", path, `{"emoji":"👍"}`, member); r.Code != 200 {
		t.Fatalf("group member reaction: %d %s", r.Code, r.Body.String())
	}
	if r = callAPI(h, "POST", base+"/leave", "", member); r.Code != 204 {
		t.Fatalf("leave: %d %s", r.Code, r.Body.String())
	}
	for _, method := range []string{"PUT", "DELETE"} {
		if r = callAPI(h, method, path, `{"emoji":"👍"}`, member); r.Code != 404 {
			t.Fatalf("former member %s: %d %s", method, r.Code, r.Body.String())
		}
	}
	if r = callAPI(h, "DELETE", base, "", owner); r.Code != 204 {
		t.Fatalf("delete chat: %d %s", r.Code, r.Body.String())
	}
	for _, method := range []string{"PUT", "DELETE"} {
		if r = callAPI(h, method, path, `{"emoji":"👍"}`, owner); r.Code != 404 {
			t.Fatalf("deleted chat %s: %d %s", method, r.Code, r.Body.String())
		}
	}
	if _, err := db.Exec(t.Context(), `DELETE FROM messages WHERE id=$1`, message.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM message_reactions WHERE message_id=$1`, message.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cascading message deletion: %d %v", count, err)
	}
}
