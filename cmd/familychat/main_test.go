package main

import (
	"context"
	"os"
	"testing"
	"time"

	"familychat/server/internal/database"
)

func TestBootstrapCreatesAdminOnce(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	login := "bootstrap_test"
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE login=$1`, login) }()
	if err := bootstrapAdmin(ctx, pool, login, "bootstrap secure password"); err != nil {
		t.Fatal(err)
	}
	var role string
	if err := pool.QueryRow(ctx, `SELECT role FROM users WHERE login=$1`, login).Scan(&role); err != nil || role != "admin" {
		t.Fatalf("role=%q err=%v", role, err)
	}
	if err := bootstrapAdmin(ctx, pool, "another_bootstrap", "bootstrap secure password"); err == nil {
		t.Fatal("second bootstrap succeeded")
	}
}
