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
	"familychat/server/internal/database"
	"familychat/server/internal/httpapi"
	"familychat/server/internal/objects"
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
		return errors.New("usage: familychat serve|migrate|bootstrap-admin|worker")
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
	if args[0] != "serve" && args[0] != "migrate" && args[0] != "worker" {
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
	if args[0] == "migrate" {
		return nil
	}
	if args[0] == "worker" {
		return errors.New("worker is not implemented yet")
	}
	store := objects.New(c)
	service := &auth.Service{DB: pool, TTL: c.SessionTTL}
	h := httpapi.New(httpapi.Dependencies{DB: pool, Objects: store, Auth: service, MaxFileBytes: c.MaxFileBytes, UserQuotaBytes: c.UserQuotaBytes})
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
