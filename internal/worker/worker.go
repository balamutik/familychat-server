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
		FROM preview_jobs j JOIN attachments a ON a.id=j.attachment_id
		WHERE (j.state='pending' AND (j.lease_until IS NULL OR j.lease_until<now())) OR (j.state='processing' AND j.lease_until<now())
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
	if state == "ready" || state == "failed" || state == "unsupported" {
		if _, err = final.Exec(ctx, `INSERT INTO events(chat_id,kind,payload) VALUES($1,'preview',jsonb_build_object('attachment_id',$2::text,'state',$3::text))`, j.ChatID, j.AttachmentID, state); err != nil {
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
	return nil
}
