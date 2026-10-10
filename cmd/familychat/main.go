package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"familychat/server/internal/auth"
	"familychat/server/internal/config"
	"familychat/server/internal/contentcrypto"
	"familychat/server/internal/database"
	"familychat/server/internal/events"
	"familychat/server/internal/httpapi"
	"familychat/server/internal/objects"
	"familychat/server/internal/push"
	"familychat/server/internal/worker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: familychat serve|migrate|bootstrap-admin|worker|healthcheck|prepare-content-secrets")
	}
	if args[0] == "healthcheck" {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:8080/health/ready")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("health status %d", resp.StatusCode)
		}
		return nil
	}
	if args[0] == "bootstrap-admin" {
		url := os.Getenv("DATABASE_URL")
		if url == "" {
			return errors.New("DATABASE_URL is required")
		}
		passwordFile := os.Getenv("ADMIN_PASSWORD_FILE")
		if passwordFile == "" {
			return errors.New("ADMIN_PASSWORD_FILE is required")
		}
		password, err := os.ReadFile(passwordFile)
		if err != nil {
			return errors.New("cannot read ADMIN_PASSWORD_FILE")
		}
		login := os.Getenv("ADMIN_LOGIN")
		if login == "" {
			return errors.New("ADMIN_LOGIN is required")
		}
		pool, err := database.Open(ctx, url)
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := database.Migrate(ctx, pool); err != nil {
			return err
		}
		return bootstrapAdmin(ctx, pool, login, strings.TrimSuffix(strings.TrimSuffix(string(password), "\n"), "\r"))
	}
	if args[0] != "serve" && args[0] != "migrate" && args[0] != "worker" && args[0] != "prepare-content-secrets" {
		return errors.New("unknown command")
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	pool, err := database.Open(ctx, c.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		return err
	}
	if args[0] == "prepare-content-secrets" {
		return contentcrypto.PrepareSecrets(ctx, pool, "/run/content-secrets", "/legacy-keys/key.json")
	}
	if args[0] == "migrate" {
		return nil
	}
	store := objects.New(c)
	if args[0] == "worker" {
		pushClient, err := push.New(c)
		if err != nil {
			return err
		}
		stopCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		return (&worker.Worker{DB: pool, Objects: store, Push: pushClient}).Run(stopCtx)
	}
	log.Print("Checking content encryption and migrating legacy data")
	var contentKey *contentcrypto.Key
	if keyPath := os.Getenv("CONTENT_KEY_FILE"); keyPath != "" {
		// Explicit compatibility option for non-Docker deployments.
		contentKey, err = contentcrypto.Initialize(ctx, pool, store, keyPath)
	} else {
		contentKey, err = contentcrypto.InitializeSecrets(ctx, pool, store,
			"/run/secrets/content_public_key", "/run/secrets/content_private_key")
	}
	if err != nil {
		return fmt.Errorf("content encryption initialization: %w", err)
	}
	service := &auth.Service{DB: pool, TTL: c.SessionTTL}
	hub := events.NewHub(pool)
	hubCtx, cancelHub := context.WithCancel(ctx)
	defer cancelHub()
	go func() {
		if err := hub.Run(hubCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("event delivery stopped: %v", err)
		}
	}()
	// A single API instance owns active calls; a restart ends stale signaling sessions.
	if _, err := pool.Exec(ctx, `WITH ended AS (UPDATE calls SET state=CASE WHEN state='ringing' THEN 'missed' ELSE 'ended' END,ended_at=now() WHERE state IN ('ringing','accepted') RETURNING id,chat_id) INSERT INTO events(chat_id,kind,payload) SELECT chat_id,'call_server_restart',jsonb_build_object('call_id',id::text) FROM ended`); err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-hubCtx.Done():
				return
			case <-ticker.C:
				if err := httpapi.SweepCalls(hubCtx, pool); err != nil {
					log.Printf("call sweep failed: %v", err)
				}
			}
		}
	}()
	h := httpapi.New(httpapi.Dependencies{ContentKey: contentKey, DB: pool, Objects: store, Auth: service, Events: hub, AllowedOrigins: c.AllowedOrigins, TurnURL: c.TurnURL, TurnSecret: c.TurnSecret, PushEnabled: c.APNsKeyFile != "", MaxFileBytes: c.MaxFileBytes, UserQuotaBytes: c.UserQuotaBytes, AdminStaticDir: os.Getenv("ADMIN_STATIC_DIR")})
	srv := &http.Server{Addr: c.ListenAddr, Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-signalCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("FamilyChat API listening on %s", c.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func bootstrapAdmin(ctx context.Context, pool *pgxpool.Pool, login, password string) error {
	login, err := auth.NormalizeLogin(login)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT registration_enabled FROM settings WHERE singleton=true FOR UPDATE`); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin'`).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("administrator already exists")
	}
	if _, err = tx.Exec(ctx, `INSERT INTO users(login,password_hash,role) VALUES($1,$2,'admin')`, login, hash); err != nil {
		return errors.New("cannot create administrator")
	}
	return tx.Commit(ctx)
}
