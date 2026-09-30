package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestUploadFilename(t *testing.T) {
	for _, tc := range []struct {
		name, legacy, encoded, want string
		invalid                     bool
	}{
		{name: "legacy ascii", legacy: "notes.txt", want: "notes.txt"},
		{name: "legacy utf8", legacy: "привет.txt", want: "привет.txt"},
		{name: "mobile utf8", legacy: "______.txt", encoded: "UTF-8''%D0%BF%D1%80%D0%B8%D0%B2%D0%B5%D1%82.txt", want: "привет.txt"},
		{name: "literal plus", encoded: "UTF-8''a+b%20c.txt", want: "a+b c.txt"},
		{name: "invalid percent", encoded: "UTF-8''%ZZ", invalid: true},
		{name: "invalid utf8", encoded: "UTF-8''%ff", invalid: true},
		{name: "unknown charset", encoded: "latin1''name", invalid: true},
		{name: "newline", encoded: "UTF-8''file%0D%0Aevil", invalid: true},
		{name: "nul", encoded: "UTF-8''file%00", invalid: true},
		{name: "oversize", encoded: "UTF-8''" + strings.Repeat("a", 256), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			h.Set("X-File-Name", tc.legacy)
			h.Set("X-File-Name*", tc.encoded)
			got, err := uploadFilename(h)
			if (err != nil) != tc.invalid || (!tc.invalid && got != tc.want) {
				t.Fatalf("got %q err=%v; want %q invalid=%v", got, err, tc.want, tc.invalid)
			}
		})
	}
}
