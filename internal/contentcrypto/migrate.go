package contentcrypto

import (
	"context"
	"errors"
	"io"
	"os"

	"familychat/server/internal/objects"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store interface {
	Get(context.Context, string, string) (objects.Object, error)
	Put(context.Context, string, io.Reader, int64, string) error
	Delete(context.Context, string) error
}

// Initialize runs before the API accepts traffic. Each row is a checkpoint;
// interrupted conversion can safely resume. It never serves a mixed plaintext
// and ciphertext database, or generates a replacement key for an existing one.
func Initialize(ctx context.Context, db *pgxpool.Pool, store Store, path string) (*Key, error) {
	return initialize(ctx, db, store, func(expected string) (*Key, error) { return LoadOrCreate(path, expected) })
}

func InitializeSecrets(ctx context.Context, db *pgxpool.Pool, store Store, publicPath, privatePath string) (*Key, error) {
	return initialize(ctx, db, store, func(expected string) (*Key, error) { return LoadSecrets(publicPath, privatePath, expected) })
}

func initialize(ctx context.Context, db *pgxpool.Pool, store Store, load func(string) (*Key, error)) (*Key, error) {
	conn, err := db.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock(736429182)`); err != nil {
		return nil, err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(736429182)`)
	var expected string
	err = conn.QueryRow(ctx, `SELECT key_id FROM content_encryption WHERE singleton`).Scan(&expected)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	key, err := load(expected)
	if err != nil {
		return nil, err
	}
	if _, err = conn.Exec(ctx, `INSERT INTO content_encryption(singleton,key_id) VALUES(true,$1) ON CONFLICT DO NOTHING`, key.ID); err != nil {
		return nil, err
	}
	if err := key.Migrate(ctx, db, store); err != nil {
		return nil, err
	}
	if _, err = conn.Exec(ctx, `UPDATE content_encryption SET migrated=true WHERE singleton`); err != nil {
		return nil, err
	}
	return key, nil
}

func (k *Key) Migrate(ctx context.Context, db *pgxpool.Pool, store Store) error {
	for {
		var id, chat, sender, client, body string
		err := db.QueryRow(ctx, `SELECT id::text,chat_id::text,sender_id::text,client_message_id::text,body FROM messages WHERE NOT encrypted ORDER BY id LIMIT 1`).Scan(&id, &chat, &sender, &client, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		sealed, err := k.EncryptText(body, MessageContext(chat, sender, client))
		if err != nil {
			return err
		}
		if _, err := db.Exec(ctx, `UPDATE messages SET body=$2,encrypted=true WHERE id=$1 AND NOT encrypted`, id, sealed); err != nil {
			return err
		}
	}
	for {
		more, err := k.migrateFile(ctx, db, store)
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	// Delete old objects only after the new object references are committed. A
	// durable queue closes the crash window between the database and S3.
	for {
		var key string
		err := db.QueryRow(ctx, `SELECT object_key FROM encryption_gc ORDER BY object_key LIMIT 1`).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := store.Delete(ctx, key); err != nil {
			return err
		}
		if _, err := db.Exec(ctx, `DELETE FROM encryption_gc WHERE object_key=$1`, key); err != nil {
			return err
		}
	}
}

func (k *Key) migrateFile(ctx context.Context, db *pgxpool.Pool, store Store) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id, chat, object, preview, state string
	var purged bool
	err = tx.QueryRow(ctx, `SELECT id::text,chat_id::text,object_key,COALESCE(preview_key,''),preview_state,purged_at IS NOT NULL FROM attachments WHERE NOT encrypted ORDER BY id LIMIT 1 FOR UPDATE`).Scan(&id, &chat, &object, &preview, &state, &purged)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	newObject, newPreview := object, ""
	var previewSize int64
	if !purged {
		if store == nil {
			return false, errors.New("storage unavailable during encryption migration")
		}
		newObject = object + ".fc1"
		if _, err := k.convertObject(ctx, store, object, newObject, "file:"+chat); err != nil {
			return false, err
		}
		if preview != "" {
			newPreview = preview + ".fc1"
			if previewSize, err = k.convertObject(ctx, store, preview, newPreview, "preview:"+chat); err != nil {
				return false, err
			}
		}
	}
	if newPreview == "" {
		state = "unsupported"
	}
	if _, err = tx.Exec(ctx, `UPDATE attachments SET encrypted=true,object_key=$2,preview_key=NULLIF($3,''),preview_state=$4,preview_size_bytes=$5 WHERE id=$1`, id, newObject, newPreview, state, previewSize); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM preview_jobs WHERE attachment_id=$1`, id); err != nil {
		return false, err
	}
	for _, old := range []string{object, preview} {
		if old != "" {
			if _, err = tx.Exec(ctx, `INSERT INTO encryption_gc(object_key) VALUES($1) ON CONFLICT DO NOTHING`, old); err != nil {
				return false, err
			}
		}
	}
	return true, tx.Commit(ctx)
}

func (k *Key) convertObject(ctx context.Context, store Store, old, next, context string) (int64, error) {
	obj, err := store.Get(ctx, old, "")
	if err != nil {
		return 0, err
	}
	defer obj.Body.Close()
	file, err := os.CreateTemp("", "familychat-encrypted-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = k.Encrypt(file, obj.Body, obj.Metadata.Size, context); err != nil {
		return 0, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	size := StoredSize(obj.Metadata.Size)
	return size, store.Put(ctx, next, file, size, "application/octet-stream")
}
