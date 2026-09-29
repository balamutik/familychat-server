package httpapi

import (
	"context"
	"io"
	"net/http"
	"time"

	"familychat/server/internal/auth"
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
	DB             *pgxpool.Pool
	Objects        FileStore
	Auth           *auth.Service
	MaxFileBytes   int64
	UserQuotaBytes int64
	Events         *events.Hub
	AllowedOrigins []string
	TurnURL        string
	TurnSecret     string
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
	registerChats(mux, deps)
	registerUsers(mux, deps)
	registerMessages(mux, deps)
	registerFiles(mux, deps)
	registerWebsocket(mux, deps)
	registerCalls(mux, deps)
	registerAdminUI(mux, deps.AdminStaticDir)
	return mux
}

func require(s *auth.Service, h http.Handler) http.Handler {
	if s == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		})
	}
	return s.Require(h)
}
