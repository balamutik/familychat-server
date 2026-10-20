package httpapi

import "testing"

func TestValidReactionEmojiRejectsMalformedSequences(t *testing.T) {
	for _, tc := range []struct {
		emoji string
		valid bool
	}{
		{"👍🏽", true}, {"👩🏽‍💻", true}, {"🇷🇺", true}, {"*️⃣", true},
		{"\xff", false}, {"👍🏽🏽", false}, {"👍️️", false}, {"😀‍a", false},
		{"🇷🇺🇺", false}, {"x️⃣", false}, {"1️", false}, {"🏴\U000e0067", false},
		{"\U000e0067", false}, {"©", true}, {"→", false}, {"☁", true},
		{"♻️", true}, {"♾️", true}, {"♿", true},
	} {
		if got := validReactionEmoji(tc.emoji); got != tc.valid {
			t.Errorf("validReactionEmoji(%q)=%t want %t", tc.emoji, got, tc.valid)
		}
	}
}
