package httpapi

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"familychat/server/internal/auth"
)

type credentials struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, 400, "invalid_json")
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		writeError(w, 400, "invalid_json")
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

type limiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.attempts == nil {
		l.attempts = make(map[string][]time.Time)
	}
	now := time.Now()
	// Expire old keys so unauthenticated requests cannot grow the map indefinitely.
	if len(l.attempts) > 10000 {
		for k, v := range l.attempts {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > 5*time.Minute {
				delete(l.attempts, k)
			}
		}
	}
	v := l.attempts[key]
	keep := v[:0]
	for _, at := range v {
		if now.Sub(at) < 5*time.Minute {
			keep = append(keep, at)
		}
	}
	if len(keep) >= 20 {
		l.attempts[key] = keep
		return false
	}
	l.attempts[key] = append(keep, now)
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func registerAuth(mux *http.ServeMux, d Dependencies) {
	var throttle limiter
	mux.HandleFunc("POST /api/v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
		if d.Auth == nil {
			writeError(w, 503, "unavailable")
			return
		}
		if !throttle.allow("register:" + clientIP(r)) {
			writeError(w, 429, "rate_limited")
			return
		}
		var c credentials
		if !decodeJSON(w, r, &c) {
			return
		}
		p, err := d.Auth.Register(r.Context(), c.Login, c.Password)
		switch {
		case errors.Is(err, auth.ErrRegistrationClosed):
			writeError(w, 403, "registration_closed")
		case errors.Is(err, auth.ErrConflict):
			writeError(w, 409, "login_exists")
		case errors.Is(err, auth.ErrInvalidInput):
			writeError(w, 400, "invalid_credentials")
		case err != nil:
			writeError(w, 500, "internal")
		default:
			writeJSON(w, 201, p)
		}
	})
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if d.Auth == nil {
			writeError(w, 503, "unavailable")
			return
		}
		var c credentials
		if !decodeJSON(w, r, &c) {
			return
		}
		if !throttle.allow("login:" + clientIP(r) + ":" + strings.ToLower(c.Login)) {
			writeError(w, 429, "rate_limited")
			return
		}
		token, p, err := d.Auth.Login(r.Context(), c.Login, c.Password)
		if err != nil {
			writeError(w, 401, "invalid_credentials")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"token": token, "user": p})
	})
	mux.Handle("POST /api/v1/auth/logout", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		if err := d.Auth.RevokeSession(r.Context(), p.SessionID); err != nil {
			writeError(w, 500, "internal")
			return
		}
		w.WriteHeader(204)
	})))
	mux.Handle("POST /api/v1/auth/change-password", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Current string `json:"current_password"`
			New     string `json:"new_password"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		err := d.Auth.ChangePassword(r.Context(), p, body.Current, body.New)
		if errors.Is(err, auth.ErrInvalidCredentials) {
			writeError(w, 401, "invalid_credentials")
			return
		}
		if errors.Is(err, auth.ErrInvalidInput) {
			writeError(w, 400, "invalid_password")
			return
		}
		if err != nil {
			writeError(w, 500, "internal")
			return
		}
		w.WriteHeader(204)
	})))
	mux.Handle("GET /api/v1/auth/me", require(d.Auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		writeJSON(w, 200, p)
	})))
	registerAdmin(mux, d)
}

func parseLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return 50
	}
	if n > 100 {
		return 100
	}
	return n
}
