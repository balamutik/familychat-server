package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func registerAdminUI(mux *http.ServeMux, dir string) {
	if dir == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return
	}
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusPermanentRedirect)
	})
	mux.HandleFunc("GET /admin/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/admin/")
		if name == "" {
			name = "index.html"
		}
		clean := filepath.Clean(name)
		if clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
			http.NotFound(w, r)
			return
		}
		path := filepath.Join(dir, clean)
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			if strings.Contains(filepath.Base(name), ".") {
				http.NotFound(w, r)
				return
			}
			path = filepath.Join(dir, "index.html")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFile(w, r, path)
	})
}
