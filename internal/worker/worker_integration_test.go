package worker

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"testing"

	"familychat/server/internal/config"
	"familychat/server/internal/database"
	"familychat/server/internal/objects"
)

func TestPreviewAndCleanup(t *testing.T) {
	url, endpoint := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_S3_ENDPOINT")
	if url == "" || endpoint == "" {
		t.Skip("set integration test services")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := objects.New(config.Config{S3Endpoint: endpoint, S3Region: "us-east-1", S3Bucket: "familychat-test", S3AccessKey: "familychat_test", S3SecretKey: "familychat_test_secret", S3UsePathStyle: true})
	var userID, chatID, fileID string
	err = db.QueryRow(ctx, `INSERT INTO users(login,password_hash) VALUES('previewtest'||substring(replace(gen_random_uuid()::text,'-','') from 1 for 16),'hash') RETURNING id::text`).Scan(&userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID) })
	err = db.QueryRow(ctx, `INSERT INTO chats(kind,title) VALUES('group','test') RETURNING id::text`).Scan(&chatID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM chats WHERE id=$1`, chatID) })
	_, err = db.Exec(ctx, `INSERT INTO chat_members(chat_id,user_id,role) VALUES($1,$2,'owner')`, chatID, userID)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 20, B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	key := "tests/preview-" + chatID
	if err := store.Put(ctx, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()), "image/png"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Delete(context.Background(), key)
		_ = store.Delete(context.Background(), "previews/"+fileID+".png")
	})
	err = db.QueryRow(ctx, `INSERT INTO attachments(chat_id,uploader_id,object_key,filename,content_type,size_bytes) VALUES($1,$2,$3,'image.png','image/png',$4) RETURNING id::text`, chatID, userID, key, buf.Len()).Scan(&fileID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO preview_jobs(attachment_id) VALUES($1)`, fileID)
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{DB: db, Objects: store}
	var state, previewKey string
	for i := 0; i < 20; i++ {
		if err := w.Once(ctx); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `SELECT preview_state,COALESCE(preview_key,'') FROM attachments WHERE id=$1`, fileID).Scan(&state, &previewKey); err != nil {
			t.Fatal(err)
		}
		if state == "ready" || state == "failed" {
			break
		}
	}
	if state != "ready" {
		t.Fatalf("state=%s key=%s err=%v", state, previewKey, err)
	}
	if meta, err := store.Head(ctx, previewKey); err != nil || meta.ContentType != "image/png" || meta.Size == 0 {
		t.Fatalf("preview object=%+v err=%v", meta, err)
	}
	videoFile, err := os.CreateTemp("", "familychat-video-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	videoFile.Close()
	defer os.Remove(videoFile.Name())
	command := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "color=c=blue:s=64x64:d=1", "-frames:v", "1", videoFile.Name())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("video fixture: %v %s", err, output)
	}
	video, err := os.ReadFile(videoFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	videoKey := "tests/video-" + chatID
	if err := store.Put(ctx, videoKey, bytes.NewReader(video), int64(len(video)), "video/mp4"); err != nil {
		t.Fatal(err)
	}
	var videoID string
	err = db.QueryRow(ctx, `INSERT INTO attachments(chat_id,uploader_id,object_key,filename,content_type,size_bytes) VALUES($1,$2,$3,'video.mp4','video/mp4',$4) RETURNING id::text`, chatID, userID, videoKey, len(video)).Scan(&videoID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Delete(context.Background(), videoKey)
		_ = store.Delete(context.Background(), "previews/"+videoID+".jpg")
	})
	if _, err := db.Exec(ctx, `INSERT INTO preview_jobs(attachment_id) VALUES($1)`, videoID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := w.Once(ctx); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `SELECT preview_state,COALESCE(preview_key,'') FROM attachments WHERE id=$1`, videoID).Scan(&state, &previewKey); err != nil {
			t.Fatal(err)
		}
		if state == "ready" || state == "failed" {
			break
		}
	}
	if state != "ready" {
		t.Fatalf("video preview state=%s", state)
	}
	if meta, err := store.Head(ctx, previewKey); err != nil || meta.ContentType != "image/jpeg" || meta.Size == 0 {
		t.Fatalf("video preview=%+v err=%v", meta, err)
	}
}
