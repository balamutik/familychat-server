package worker

import (
	"context"
	"familychat/server/internal/database"
	"os"
	"slices"
	"testing"
)

func TestStoriesCleanupExpiresDeletedAndAbandonedUploadsButProtectsReaders(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var user string
	if err = db.QueryRow(ctx, `INSERT INTO users(login,password_hash) VALUES('storygc'||left(replace(gen_random_uuid()::text,'-',''),16),'hash') RETURNING id::text`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(ctx, `DELETE FROM users WHERE id=$1`, user)
	defer db.Exec(ctx, `DELETE FROM stories WHERE user_id=$1`, user)
	makeStory := func(state string, expired, deleted, abandoned bool) (string, string) {
		t.Helper()
		var id, key string
		err := db.QueryRow(ctx, `INSERT INTO stories(user_id,object_key,filename,content_type,size_bytes,stored_size_bytes,state,expires_at,deleted_at,upload_deadline)
 VALUES($1,'stories/'||gen_random_uuid()::text,'clip.mp4','video/mp4',10,10,$2,now()+CASE WHEN $3 THEN interval '-1 second' ELSE interval '24 hours' END,CASE WHEN $4 THEN now() ELSE NULL END,now()+CASE WHEN $5 THEN interval '-1 second' ELSE interval '30 minutes' END) RETURNING id::text,object_key`, user, state, expired, deleted, abandoned).Scan(&id, &key)
		if err != nil {
			t.Fatal(err)
		}
		return id, key
	}
	_, expiredKey := makeStory("ready", true, false, false)
	_, deletedKey := makeStory("ready", false, true, false)
	_, abandonedKey := makeStory("uploading", false, false, true)
	_, activeKey := makeStory("ready", false, false, false)
	_, uploadKey := makeStory("uploading", false, false, false)
	leasedID, leasedKey := makeStory("ready", true, false, false)
	lease, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Rollback(ctx)
	if _, err = lease.Exec(ctx, `SELECT id FROM stories WHERE id=$1 FOR SHARE`, leasedID); err != nil {
		t.Fatal(err)
	}
	store := &deletionStore{}
	w := Worker{DB: db, Objects: store}
	drain := func() {
		t.Helper()
		for i := 0; i < 100; i++ {
			more, e := w.purgeStory(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if !more {
				return
			}
		}
		t.Fatal("cleanup did not drain")
	}
	drain()
	for _, key := range []string{expiredKey, deletedKey, abandonedKey} {
		if !slices.Contains(store.deleted, key) {
			t.Fatalf("not cleaned %s", key)
		}
	}
	for _, key := range []string{activeKey, uploadKey, leasedKey} {
		if slices.Contains(store.deleted, key) {
			t.Fatalf("live/leased object removed %s", key)
		}
	}
	if err = lease.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	drain()
	if !slices.Contains(store.deleted, leasedKey) {
		t.Fatal("released expired reader never cleaned")
	}
	var reserved int64
	if err = db.QueryRow(ctx, `SELECT COALESCE(sum(stored_size_bytes),0) FROM stories WHERE user_id=$1 AND purged_at IS NULL`, user).Scan(&reserved); err != nil || reserved != 20 {
		t.Fatalf("quota not released: %d %v", reserved, err)
	}
}
