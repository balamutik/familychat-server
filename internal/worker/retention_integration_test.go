package worker

import (
	"context"
	"io"
	"os"
	"testing"

	"familychat/server/internal/database"
	"familychat/server/internal/objects"
)

type deletionStore struct{ deleted []string }

func (*deletionStore) Get(context.Context, string, string) (objects.Object, error) {
	return objects.Object{}, nil
}
func (*deletionStore) Put(context.Context, string, io.Reader, int64, string) error { return nil }
func (s *deletionStore) Delete(_ context.Context, key string) error {
	s.deleted = append(s.deleted, key)
	return nil
}

func TestExpiredFilePurgeKeepsMessageRecord(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL")
	}
	ctx := t.Context()
	db, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var previousDays int
	if err := db.QueryRow(ctx, `SELECT retention_days FROM settings WHERE singleton=true`).Scan(&previousDays); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `UPDATE settings SET retention_days=$1 WHERE singleton=true`, previousDays)
	})
	if _, err := db.Exec(ctx, `UPDATE settings SET retention_days=0 WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	var userID, chatID, fileID string
	if err := db.QueryRow(ctx, `INSERT INTO users(login,password_hash) VALUES('purge'||substring(replace(gen_random_uuid()::text,'-','') from 1 for 16),'hash') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID) })
	if err := db.QueryRow(ctx, `INSERT INTO chats(kind,title) VALUES('group','retention test') RETURNING id::text`).Scan(&chatID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM chats WHERE id=$1`, chatID) })
	if _, err := db.Exec(ctx, `INSERT INTO chat_members(chat_id,user_id,role) VALUES($1,$2,'owner')`, chatID, userID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO attachments(chat_id,uploader_id,object_key,preview_key,filename,content_type,size_bytes,preview_state,created_at)
		VALUES($1,$2,$3,$4,'old.png','image/png',12,'ready',now()-interval '2 days') RETURNING id::text`, chatID, userID, "tests/old-"+chatID, "tests/old-preview-"+chatID).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	var messageID string
	if err := db.QueryRow(ctx, `INSERT INTO messages(chat_id,seq,sender_id,client_message_id,body) VALUES($1,1,$2,gen_random_uuid(),'old file') RETURNING id::text`, chatID, userID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO message_attachments(message_id,attachment_id) VALUES($1,$2)`, messageID, fileID); err != nil {
		t.Fatal(err)
	}
	store := &deletionStore{}
	w := &Worker{DB: db, Objects: store}
	if purged, err := w.purgeExpired(ctx); err != nil || purged || len(store.deleted) != 0 {
		t.Fatalf("unlimited retention purged=%v keys=%v err=%v", purged, store.deleted, err)
	}
	if _, err := db.Exec(ctx, `UPDATE settings SET retention_days=1 WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	if purged, err := w.purgeExpired(ctx); err != nil || !purged {
		t.Fatalf("purge=%v err=%v", purged, err)
	}
	if len(store.deleted) != 2 || store.deleted[0] != "tests/old-"+chatID || store.deleted[1] != "tests/old-preview-"+chatID {
		t.Fatalf("deleted keys=%v", store.deleted)
	}
	var purged, linked bool
	if err := db.QueryRow(ctx, `SELECT purged_at IS NOT NULL FROM attachments WHERE id=$1`, fileID).Scan(&purged); err != nil || !purged {
		t.Fatalf("purged=%v err=%v", purged, err)
	}
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM message_attachments WHERE message_id=$1 AND attachment_id=$2)`, messageID, fileID).Scan(&linked); err != nil || !linked {
		t.Fatalf("message link=%v err=%v", linked, err)
	}
	if purgedAgain, err := w.purgeExpired(ctx); err != nil || purgedAgain {
		t.Fatalf("repeat purge=%v err=%v", purgedAgain, err)
	}
}

func TestUnattachedFileIsPurgedWithUnlimitedRetention(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL")
	}
	ctx := t.Context()
	db, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var previousDays int
	if err := db.QueryRow(ctx, `SELECT retention_days FROM settings WHERE singleton=true`).Scan(&previousDays); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `UPDATE settings SET retention_days=$1 WHERE singleton=true`, previousDays)
	})
	if _, err := db.Exec(ctx, `UPDATE settings SET retention_days=0 WHERE singleton=true`); err != nil {
		t.Fatal(err)
	}
	var userID, chatID, fileID string
	if err := db.QueryRow(ctx, `INSERT INTO users(login,password_hash) VALUES('orphan'||substring(replace(gen_random_uuid()::text,'-','') from 1 for 16),'hash') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID) })
	if err := db.QueryRow(ctx, `INSERT INTO chats(kind,title) VALUES('group','orphan test') RETURNING id::text`).Scan(&chatID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM chats WHERE id=$1`, chatID) })
	if _, err := db.Exec(ctx, `INSERT INTO chat_members(chat_id,user_id,role) VALUES($1,$2,'owner')`, chatID, userID); err != nil {
		t.Fatal(err)
	}
	objectKey := "tests/orphan-" + chatID
	previewKey := "tests/orphan-preview-" + chatID
	if err := db.QueryRow(ctx, `INSERT INTO attachments(chat_id,uploader_id,object_key,preview_key,filename,content_type,size_bytes,preview_state,created_at)
		VALUES($1,$2,$3,$4,'orphan.png','image/png',12,'ready',now()-interval '2 days') RETURNING id::text`,
		chatID, userID, objectKey, previewKey).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	store := &deletionStore{}
	w := &Worker{DB: db, Objects: store}
	if purged, err := w.purgeExpired(ctx); err != nil || !purged {
		t.Fatalf("orphan purge=%v err=%v", purged, err)
	}
	if len(store.deleted) != 2 || store.deleted[0] != objectKey || store.deleted[1] != previewKey {
		t.Fatalf("deleted keys=%v", store.deleted)
	}
	var purged bool
	if err := db.QueryRow(ctx, `SELECT purged_at IS NOT NULL FROM attachments WHERE id=$1`, fileID).Scan(&purged); err != nil || !purged {
		t.Fatalf("purged=%v err=%v", purged, err)
	}
	if again, err := w.purgeExpired(ctx); err != nil || again {
		t.Fatalf("repeat purge=%v err=%v", again, err)
	}
}
