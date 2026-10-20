package httpapi

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"familychat/server/internal/auth"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rivo/uniseg"
)

type reactionView struct {
	Emoji   string   `json:"emoji"`
	UserIDs []string `json:"user_ids"`
}

func registerMessageReactions(mux *http.ServeMux, d Dependencies) {
	for _, method := range []string{"PUT", "DELETE"} {
		mux.Handle(method+" /api/v1/chats/{id}/messages/{messageID}/reaction", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			chatID, id := r.PathValue("id"), r.PathValue("messageID")
			if !validUUID(chatID) || !validUUID(id) {
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
			// Match the chat/member/message lock order used by edit, hide and delete.
			if _, _, err = chatRole(r, tx, chatID, p.UserID); err != nil {
				writeError(w, 404, "not_found")
				return
			}
			var v messageView
			err = tx.QueryRow(r.Context(), `SELECT id::text,chat_id::text,seq,sender_id::text,client_message_id::text,body,encrypted,created_at::text
				FROM messages WHERE chat_id=$1 AND id=$2 AND deleted_at IS NULL
				AND NOT EXISTS(SELECT 1 FROM message_hidden mh WHERE mh.message_id=messages.id AND mh.user_id=$3) FOR UPDATE`, chatID, id, p.UserID).Scan(&v.ID, &v.ChatID, &v.Seq, &v.SenderID, &v.ClientMessageID, &v.Text, &v.Encrypted, &v.CreatedAt)
			if err != nil {
				writeError(w, 404, "not_found")
				return
			}
			var changed pgconn.CommandTag
			if r.Method == "PUT" {
				if !requireDirectCommunication(w, r, tx, chatID) {
					return
				}
				var body struct {
					Emoji string `json:"emoji"`
				}
				if !decodeJSON(w, r, &body) {
					return
				}
				if !validReactionEmoji(body.Emoji) {
					writeError(w, 400, "invalid_reaction")
					return
				}
				changed, err = tx.Exec(r.Context(), `INSERT INTO message_reactions(message_id,user_id,emoji) VALUES($1,$2,$3)
					ON CONFLICT(message_id,user_id) DO UPDATE SET emoji=EXCLUDED.emoji WHERE message_reactions.emoji<>EXCLUDED.emoji`, id, p.UserID, body.Emoji)
			} else {
				// Removing an existing reaction remains possible after either peer blocks.
				changed, err = tx.Exec(r.Context(), `DELETE FROM message_reactions WHERE message_id=$1 AND user_id=$2`, id, p.UserID)
			}
			if err == nil && changed.RowsAffected() > 0 {
				_, err = tx.Exec(r.Context(), `INSERT INTO events(chat_id,kind,payload) VALUES($1,'message_updated',jsonb_build_object('message_id',$2::text,'seq',$3::bigint))`, chatID, id, v.Seq)
			}
			if err != nil {
				writeError(w, 500, "internal")
				return
			}
			if err = tx.Commit(r.Context()); err != nil {
				writeError(w, 500, "internal")
				return
			}
			if err = attachMetadata(r.Context(), d.DB, []*messageView{&v}); err != nil {
				writeError(w, 500, "internal")
				return
			}
			writeJSON(w, 200, v)
		})))
	}
}

// Reactions preserve the exact device emoji sequence. Bound its byte size and
// grapheme count, then reject text, controls, isolated modifiers and incomplete
// sequences. ZWJ compositions are not restricted to a fixed picker catalogue.
func validReactionEmoji(s string) bool {
	if len(s) == 0 || len(s) > 128 || !utf8.ValidString(s) || uniseg.GraphemeClusterCount(s) != 1 {
		return false
	}
	runes := []rune(s)
	if first := runes[0]; first == '#' || first == '*' || (first >= '0' && first <= '9') {
		return (len(runes) == 2 && runes[1] == 0x20e3) || (len(runes) == 3 && runes[1] == 0xfe0f && runes[2] == 0x20e3)
	}
	if runes[0] >= 0x1f1e6 && runes[0] <= 0x1f1ff {
		return len(runes) == 2 && runes[1] >= 0x1f1e6 && runes[1] <= 0x1f1ff
	}
	if runes[0] == 0x1f3f4 && len(runes) > 1 && runes[1] >= 0xe0061 && runes[1] <= 0xe007a {
		if len(runes) < 4 || runes[len(runes)-1] != 0xe007f {
			return false
		}
		for _, r := range runes[1 : len(runes)-1] {
			if r < 0xe0061 || r > 0xe007a {
				return false
			}
		}
		return true
	}
	for _, segment := range strings.Split(s, "\u200d") {
		r := []rune(segment)
		if len(r) == 0 || !reactionEmojiBase(r[0]) {
			return false
		}
		i := 1
		if i < len(r) && r[i] == 0xfe0f {
			i++
		}
		if i < len(r) && r[i] >= 0x1f3fb && r[i] <= 0x1f3ff {
			i++
		}
		if i != len(r) {
			return false
		}
	}
	return true
}

func reactionEmojiBase(r rune) bool {
	if r >= 0x1f300 && r <= 0x1faff {
		return r < 0x1f3fb || r > 0x1f3ff
	}
	switch r {
	case 0x00a9, 0x00ae, 0x203c, 0x2049, 0x2122, 0x2139,
		0x2328, 0x23cf, 0x24c2, 0x25b6, 0x25c0, 0x260e, 0x2611,
		0x2618, 0x261d, 0x2620, 0x2626, 0x262a, 0x2638, 0x2640, 0x2642,
		0x2663, 0x2668, 0x267b, 0x267e, 0x267f, 0x2699, 0x26a7, 0x26c8, 0x26ce, 0x26cf, 0x26d1,
		0x26fd, 0x2702, 0x2705, 0x270f, 0x2712, 0x2714, 0x2716, 0x271d,
		0x2721, 0x2728, 0x2744, 0x2747, 0x274c, 0x274e, 0x2757,
		0x27a1, 0x27b0, 0x27bf, 0x2b50, 0x2b55, 0x3030, 0x303d, 0x3297, 0x3299,
		0x1f004, 0x1f0cf, 0x1f170, 0x1f171, 0x1f17e, 0x1f17f, 0x1f18e,
		0x1f201, 0x1f202, 0x1f21a, 0x1f22f, 0x1f250, 0x1f251:
		return true
	}
	for _, pair := range [][2]rune{
		{0x2194, 0x2199}, {0x21a9, 0x21aa}, {0x231a, 0x231b},
		{0x23e9, 0x23f3}, {0x23f8, 0x23fa}, {0x25aa, 0x25ab}, {0x25fb, 0x25fe},
		{0x2600, 0x2604}, {0x2614, 0x2615}, {0x2622, 0x2623}, {0x262e, 0x262f},
		{0x2639, 0x263a}, {0x2648, 0x2653}, {0x265f, 0x2660}, {0x2665, 0x2666},
		{0x2692, 0x2697}, {0x269b, 0x269c}, {0x26a0, 0x26a1}, {0x26aa, 0x26ab},
		{0x26b0, 0x26b1}, {0x26bd, 0x26be}, {0x26c4, 0x26c5}, {0x26d3, 0x26d4},
		{0x26e9, 0x26ea}, {0x26f0, 0x26f5}, {0x26f7, 0x26fa}, {0x2708, 0x270d},
		{0x2733, 0x2734}, {0x2753, 0x2755}, {0x2763, 0x2764}, {0x2795, 0x2797},
		{0x2934, 0x2935}, {0x2b05, 0x2b07}, {0x2b1b, 0x2b1c},
		{0x1f191, 0x1f19a}, {0x1f232, 0x1f23a},
	} {
		if r >= pair[0] && r <= pair[1] {
			return true
		}
	}
	return false
}
