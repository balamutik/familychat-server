package contentcrypto

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"familychat/server/internal/objects"
	"github.com/jackc/pgx/v5/pgxpool"
)

type memoryStore struct {
	data       map[string][]byte
	failDelete bool
}

func (s *memoryStore) Get(_ context.Context, k, _ string) (objects.Object, error) {
	b, ok := s.data[k]
	if !ok {
		return objects.Object{}, errors.New("missing object")
	}
	return objects.Object{Body: io.NopCloser(bytes.NewReader(b)), Metadata: objects.Metadata{Size: int64(len(b))}}, nil
}
func (s *memoryStore) Put(_ context.Context, k string, r io.Reader, n int64, _ string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != n {
		return errors.New("size mismatch")
	}
	s.data[k] = b
	return nil
}
func (s *memoryStore) Delete(_ context.Context, k string) error {
	if s.failDelete {
		return errors.New("temporary S3 error")
	}
	delete(s.data, k)
	return nil
}

func TestLegacyMigrationResumesAndPreservesKey(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL")
	}
	ctx := t.Context()
	root, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	schema := fmt.Sprintf("crypto_test_%d", time.Now().UnixNano())
	if _, err = root.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer root.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(ctx, `CREATE TABLE messages(id text PRIMARY KEY,chat_id text,sender_id text,client_message_id text,body text,encrypted boolean DEFAULT false);
 CREATE TABLE attachments(id text PRIMARY KEY,chat_id text,object_key text,preview_key text,preview_state text,preview_size_bytes bigint DEFAULT 0,purged_at timestamptz,encrypted boolean DEFAULT false);
 CREATE TABLE preview_jobs(attachment_id text);
 CREATE TABLE encryption_gc(object_key text PRIMARY KEY);
 CREATE TABLE content_encryption(singleton boolean PRIMARY KEY,key_id text,migrated boolean DEFAULT false);
 INSERT INTO messages VALUES('message','chat','sender','client','Личная переписка',false);
 INSERT INTO attachments(id,chat_id,object_key,preview_key,preview_state) VALUES('file','chat','original','preview','ready');`)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{data: map[string][]byte{"original": bytes.Repeat([]byte("private file"), 200000), "preview": []byte("private preview")}, failDelete: true}
	original := append([]byte{}, store.data["original"]...)
	path := filepath.Join(t.TempDir(), "key.json")
	if _, err = Initialize(ctx, db, store, path); err == nil {
		t.Fatal("reported complete before deleting plaintext objects")
	}
	key, err := LoadOrCreate(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.data["original.fc1"]; !ok {
		t.Fatal("encrypted copy was not checkpointed")
	}
	store.failDelete = false
	resumed, err := Initialize(ctx, db, store, path)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != key.ID {
		t.Fatal("key changed across restart")
	}
	for _, name := range []string{"original", "preview"} {
		if _, ok := store.data[name]; ok {
			t.Fatal("plaintext object survived migration")
		}
	}
	var opened bytes.Buffer
	if err := key.Decrypt(&opened, bytes.NewReader(store.data["original.fc1"]), "file:chat"); err != nil || !bytes.Equal(opened.Bytes(), original) {
		t.Fatalf("migrated file: %v", err)
	}
	opened.Reset()
	if err := key.Decrypt(&opened, bytes.NewReader(store.data["preview.fc1"]), "preview:chat"); err != nil || opened.String() != "private preview" {
		t.Fatalf("migrated preview: %v", err)
	}
	var body string
	var done bool
	if err = db.QueryRow(ctx, `SELECT body FROM messages`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	sealed, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		t.Fatal(err)
	}
	opened.Reset()
	if err = key.Decrypt(&opened, bytes.NewReader(sealed), MessageContext("chat", "sender", "client")); err != nil || opened.String() != "Личная переписка" {
		t.Fatalf("migrated text: %v", err)
	}
	if err = db.QueryRow(ctx, `SELECT migrated FROM content_encryption`).Scan(&done); err != nil || !done {
		t.Fatal("migration not complete")
	}
	if _, err = Initialize(ctx, db, store, path); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err = Initialize(ctx, db, store, path); err == nil {
		t.Fatal("lost key silently replaced")
	}
}
