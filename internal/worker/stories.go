package worker

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

// purgeStory locks one expired/deleted/stalled upload through object deletion.
// Content readers hold a shared row lease, so cleanup skips their active streams.
func (w *Worker) purgeStory(ctx context.Context) (bool, error) {
	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id, key string
	err = tx.QueryRow(ctx, `SELECT id::text,object_key FROM stories WHERE purged_at IS NULL AND
 (deleted_at IS NOT NULL OR expires_at<=now() OR state='failed' OR (state='uploading' AND upload_deadline<=now()))
 ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = w.Objects.Delete(ctx, key); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE stories SET purged_at=now() WHERE id=$1`, id); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
