package worker

import (
	"context"
	"os"
	"testing"
	"time"

	"familychat/server/internal/database"
)

func TestAvatarGarbageIsRemovedFromObjectStore(t *testing.T) {
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
	key := "tests/avatar-gc-" + time.Now().Format("20060102150405.000000000")
	if _, err := db.Exec(ctx, `INSERT INTO avatar_gc(object_key) VALUES($1)`, key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM avatar_gc WHERE object_key=$1`, key) })
	store := &deletionStore{}
	w := &Worker{DB: db, Objects: store}
	if err := w.cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, deleted := range store.deleted {
		if deleted == key {
			found = true
		}
	}
	if !found {
		t.Fatalf("deleted objects=%v, want %s", store.deleted, key)
	}
	var remaining bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM avatar_gc WHERE object_key=$1)`, key).Scan(&remaining); err != nil || remaining {
		t.Fatalf("garbage queue remained=%v err=%v", remaining, err)
	}
}
