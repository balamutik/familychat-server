package httpapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/contentcrypto"
	"familychat/server/internal/events"
	"familychat/server/internal/objects"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ObjectHealth interface{ Ping(context.Context) error }
type FileStore interface {
	ObjectHealth
	Put(context.Context, string, io.Reader, int64, string) error
	Head(context.Context, string) (objects.Metadata, error)
	Get(context.Context, string, string) (objects.Object, error)
	Delete(context.Context, string) error
}

type Dependencies struct {
	ContentKey     *contentcrypto.Key
	DB             *pgxpool.Pool
	Objects        FileStore
	Auth           *auth.Service
	MaxFileBytes   int64
	UserQuotaBytes int64
	Events         *events.Hub
	AllowedOrigins []string
	TurnURL        string
	TurnSecret     string
	PushEnabled    bool
	AdminStaticDir string
}

func New(deps Dependencies) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if deps.DB == nil || deps.Objects == nil || deps.DB.Ping(ctx) != nil || deps.Objects.Ping(ctx) != nil {
			http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	registerAuth(mux, deps)
	registerContentEncryption(mux, deps)
	registerInvites(mux, deps)
	registerChats(mux, deps)
	registerUsers(mux, deps)
	registerAvatars(mux, deps)
	registerMessages(mux, deps)
	registerFiles(mux, deps)
	registerWebsocket(mux, deps)
	registerCalls(mux, deps)
	registerPushDevices(mux, deps)
	registerAdminUI(mux, deps.AdminStaticDir)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		contentRead := r.Method == "GET" || r.Method == "HEAD"
		contentRead = contentRead && (path == "/api/v1/chats" || strings.HasPrefix(path, "/api/v1/files/") ||
			(strings.HasPrefix(path, "/api/v1/chats/") && (strings.HasSuffix(path, "/messages") || strings.Count(strings.TrimPrefix(path, "/api/v1/chats/"), "/") == 0)))
		if deps.ContentKey != nil && contentRead && r.Header.Get("X-Content-Encryption") != "fc1" {
			writeError(w, 426, "encryption_required")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func require(s *auth.Service, h http.Handler) http.Handler {
	if s == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		})
	}
	return s.Require(h)
}
