package database

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestMigrationIsRepeatable(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL migration integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
}

func TestImagePreviewMigrationRetriesOnlyMissingSupportedImages(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL migration integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var userID, chatID string
	if err := tx.QueryRow(ctx, `INSERT INTO users(login,password_hash) VALUES('img'||substr(replace(gen_random_uuid()::text,'-',''),1,16),'hash') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO chats(kind,title) VALUES('group','image migration test') RETURNING id::text`).Scan(&chatID); err != nil {
		t.Fatal(err)
	}
	type testCase struct {
		contentType, state, expected string
	}
	for _, tc := range []testCase{
		{"image/heic", "failed", "pending"},
		{"image/svg+xml", "unsupported", "pending"},
		{"image/png", "ready", "ready"},
		{"application/octet-stream", "failed", "failed"},
	} {
		var attachmentID string
		if err := tx.QueryRow(ctx, `INSERT INTO attachments(chat_id,uploader_id,object_key,filename,content_type,size_bytes,preview_state)
			VALUES($1,$2,'tests/'||gen_random_uuid()::text,'image',$3,100,$4) RETURNING id::text`, chatID, userID, tc.contentType, tc.state).Scan(&attachmentID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO preview_jobs(attachment_id,state,attempts) VALUES($1,$2,3)`, attachmentID, tc.state); err != nil {
			t.Fatal(err)
		}
		if tc.state == "ready" {
			if _, err := tx.Exec(ctx, `UPDATE attachments SET preview_key='previews/'||id::text||'.jpg' WHERE id=$1`, attachmentID); err != nil {
				t.Fatal(err)
			}
		}
	}
	body, err := migrationFiles.ReadFile("migrations/0006_retry_image_previews.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `SELECT a.content_type,a.preview_state,j.state,j.attempts FROM attachments a
		JOIN preview_jobs j ON j.attachment_id=a.id WHERE a.chat_id=$1`, chatID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	expected := map[string]testCase{
		"image/heic":               {expected: "pending"},
		"image/svg+xml":            {expected: "pending"},
		"image/png":                {expected: "ready"},
		"application/octet-stream": {expected: "failed"},
	}
	count := 0
	for rows.Next() {
		var contentType, previewState, jobState string
		var attempts int
		if err := rows.Scan(&contentType, &previewState, &jobState, &attempts); err != nil {
			t.Fatal(err)
		}
		want, ok := expected[contentType]
		if !ok || previewState != want.expected || jobState != want.expected || (want.expected == "pending" && attempts != 0) {
			t.Fatalf("%s: preview=%s job=%s attempts=%d", contentType, previewState, jobState, attempts)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != len(expected) {
		t.Fatalf("got %d attachments, want %d", count, len(expected))
	}
}
