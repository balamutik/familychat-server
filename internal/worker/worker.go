package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"familychat/server/internal/objects"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store interface {
	Get(context.Context, string, string) (objects.Object, error)
	Put(context.Context, string, io.Reader, int64, string) error
	Delete(context.Context, string) error
}

type Worker struct {
	DB      *pgxpool.Pool
	Objects Store
	FFmpeg  string
}

type job struct {
	ID, AttachmentID, ChatID, ObjectKey, ContentType string
	Attempts                                         int
}

func (w *Worker) Run(ctx context.Context) error {
	if w.FFmpeg == "" {
		w.FFmpeg = "ffmpeg"
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := w.Once(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Worker) Once(ctx context.Context) error {
	if err := w.cleanup(ctx); err != nil {
		return err
	}
	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var j job
	err = tx.QueryRow(ctx, `SELECT j.id::text,j.attachment_id::text,a.chat_id::text,a.object_key,a.content_type,j.attempts
		FROM preview_jobs j JOIN attachments a ON a.id=j.attachment_id CROSS JOIN settings s
		WHERE a.purged_at IS NULL AND (s.retention_days=0 OR a.created_at>now()-s.retention_days*interval '1 day')
		AND ((j.state='pending' AND (j.lease_until IS NULL OR j.lease_until<now())) OR (j.state='processing' AND j.lease_until<now()))
		ORDER BY j.created_at FOR UPDATE OF j SKIP LOCKED LIMIT 1`).Scan(&j.ID, &j.AttachmentID, &j.ChatID, &j.ObjectKey, &j.ContentType, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE preview_jobs SET state='processing',attempts=attempts+1,lease_until=now()+interval '2 minutes' WHERE id=$1`, j.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE attachments SET preview_state='processing' WHERE id=$1`, j.AttachmentID); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	state, key := w.makePreview(ctx, j)
	if state == "" {
		state = "failed"
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if state == "failed" && j.Attempts < 2 {
		state = "pending"
	}
	final, err := w.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer final.Rollback(ctx)
	var expired bool
	if err = final.QueryRow(ctx, `SELECT a.purged_at IS NOT NULL OR (s.retention_days>0 AND a.created_at<=now()-s.retention_days*interval '1 day') FROM attachments a CROSS JOIN settings s WHERE a.id=$1 FOR UPDATE OF a`, j.AttachmentID).Scan(&expired); err != nil {
		return err
	}
	if expired {
		if key != "" {
			if err = w.Objects.Delete(ctx, key); err != nil {
				return err
			}
		}
		state, key = "failed", ""
	}
	if _, err = final.Exec(ctx, `UPDATE preview_jobs SET state=$2,lease_until=NULL WHERE id=$1`, j.ID, state); err != nil {
		return err
	}
	if state == "ready" {
		if _, err = final.Exec(ctx, `UPDATE attachments SET preview_state='ready',preview_key=$2 WHERE id=$1`, j.AttachmentID, key); err != nil {
			_ = w.Objects.Delete(ctx, key)
			return err
		}
	} else {
		if _, err = final.Exec(ctx, `UPDATE attachments SET preview_state=$2 WHERE id=$1`, j.AttachmentID, state); err != nil {
			return err
		}
	}
	if !expired && (state == "ready" || state == "failed" || state == "unsupported") {
		if _, err = final.Exec(ctx, `INSERT INTO events(chat_id,kind,payload)
			SELECT $1,'preview',jsonb_build_object('attachment_id',$2::text,'state',$3::text)
			WHERE EXISTS(SELECT 1 FROM message_attachments WHERE attachment_id=$2::uuid)`, j.ChatID, j.AttachmentID, state); err != nil {
			return err
		}
	}
	if err = final.Commit(ctx); err != nil {
		if state == "ready" {
			_ = w.Objects.Delete(ctx, key)
		}
		return err
	}
	return nil
}

func (w *Worker) makePreview(ctx context.Context, j job) (string, string) {
	ffmpeg := w.FFmpeg
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	ctype := strings.ToLower(strings.Split(j.ContentType, ";")[0])
	if !strings.HasPrefix(ctype, "image/") && !strings.HasPrefix(ctype, "video/") {
		return "unsupported", ""
	}
	if ctype == "image/svg+xml" {
		return "unsupported", ""
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	in, err := os.CreateTemp("", "familychat-in-*")
	if err != nil {
		return "failed", ""
	}
	defer os.Remove(in.Name())
	defer in.Close()
	obj, err := w.Objects.Get(ctx, j.ObjectKey, "")
	if err != nil {
		return "failed", ""
	}
	_, err = io.Copy(in, io.LimitReader(obj.Body, 251<<20))
	_ = obj.Body.Close()
	if err != nil {
		return "failed", ""
	}
	out, err := os.CreateTemp("", "familychat-preview-*.jpg")
	if err != nil {
		return "failed", ""
	}
	out.Close()
	defer os.Remove(out.Name())
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-y", "-i", in.Name(), "-frames:v", "1", "-vf", "scale=640:640:force_original_aspect_ratio=decrease", "-q:v", "4", out.Name())
	if err = cmd.Run(); err != nil {
		return "failed", ""
	}
	stat, err := os.Stat(out.Name())
	if err != nil || stat.Size() == 0 || stat.Size() > 5<<20 {
		return "failed", ""
	}
	f, err := os.Open(out.Name())
	if err != nil {
		return "failed", ""
	}
	defer f.Close()
	key := filepath.ToSlash("previews/" + j.AttachmentID + ".jpg")
	if err = w.Objects.Put(ctx, key, f, stat.Size(), "image/jpeg"); err != nil {
		return "failed", ""
	}
	return "ready", key
}

func (w *Worker) cleanup(ctx context.Context) error {
	rows, err := w.DB.Query(ctx, `UPDATE upload_reservations SET state='failed' WHERE state='uploading' AND expires_at<now() RETURNING object_key`)
	if err != nil {
		return err
	}
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := w.Objects.Delete(ctx, key); err != nil {
			return err
		}
	}
	for i := 0; i < 25; i++ {
		purged, err := w.purgeExpired(ctx)
		if err != nil {
			return err
		}
		if !purged {
			break
		}
	}
	for i := 0; i < 25; i++ {
		removed, err := w.purgeAvatarGarbage(ctx)
		if err != nil {
			return err
		}
		if !removed {
			break
		}
	}
	return nil
}

func (w *Worker) purgeAvatarGarbage(ctx context.Context) (bool, error) {
	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var key string
	err = tx.QueryRow(ctx, `SELECT gc.object_key FROM avatar_gc gc
		WHERE NOT EXISTS(SELECT 1 FROM users u WHERE u.avatar_key=gc.object_key)
		ORDER BY gc.created_at FOR UPDATE OF gc SKIP LOCKED LIMIT 1`).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := w.Objects.Delete(deleteCtx, key); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM avatar_gc WHERE object_key=$1`, key); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (w *Worker) purgeExpired(ctx context.Context) (bool, error) {
	tx, err := w.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var retentionDays int
	// Hold this policy during deletion so a concurrent admin change cannot rescue a selected file.
	if err = tx.QueryRow(ctx, `SELECT retention_days FROM settings WHERE singleton=true FOR SHARE`).Scan(&retentionDays); err != nil {
		return false, err
	}
	if retentionDays == 0 {
		return false, nil
	}
	var id, chatID, objectKey, previewKey string
	err = tx.QueryRow(ctx, `SELECT id::text,chat_id::text,object_key,COALESCE(preview_key,'') FROM attachments
		WHERE purged_at IS NULL AND created_at<=now()-$1::integer*interval '1 day'
		ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`, retentionDays).Scan(&id, &chatID, &objectKey, &previewKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = w.Objects.Delete(deleteCtx, objectKey); err != nil {
		return false, err
	}
	if previewKey != "" {
		if err = w.Objects.Delete(deleteCtx, previewKey); err != nil {
			return false, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE attachments SET purged_at=now() WHERE id=$1`, id); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE preview_jobs SET state='failed',lease_until=NULL WHERE attachment_id=$1 AND state IN ('pending','processing')`, id); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO events(chat_id,kind,payload)
		SELECT $1,'file_expired',jsonb_build_object('attachment_id',$2::text)
		WHERE EXISTS(SELECT 1 FROM message_attachments WHERE attachment_id=$2::uuid)`, chatID, id); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
