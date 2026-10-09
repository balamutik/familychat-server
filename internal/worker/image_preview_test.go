package worker

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"familychat/server/internal/objects"
)

type imagePreviewStore struct {
	input     []byte
	output    []byte
	key       string
	mediaType string
}

func (s *imagePreviewStore) Get(_ context.Context, _ string, _ string) (objects.Object, error) {
	return objects.Object{Body: io.NopCloser(bytes.NewReader(s.input))}, nil
}

func (s *imagePreviewStore) Put(_ context.Context, key string, body io.Reader, size int64, mediaType string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return errors.New("preview size differs from stored bytes")
	}
	s.output, s.key, s.mediaType = data, key, mediaType
	return nil
}

func (s *imagePreviewStore) Delete(_ context.Context, _ string) error { return nil }

func TestImagePreviewFormatsProducePNG(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	for _, tc := range []struct {
		name, filename, mediaType, dependency string
	}{
		{"JPEG", "sample.jpg", "image/jpeg", ""},
		{"PNG", "sample.png", "image/png", ""},
		{"GIF", "sample.gif", "image/gif", ""},
		{"WebP", "sample.webp", "image/webp", ""},
		{"HEIC", "sample.heic", "image/heic", "heif-convert"},
		{"HEIF", "sample.heic", "image/heif", "heif-convert"},
		{"AVIF", "sample.avif", "image/avif", ""},
		{"BMP", "sample.bmp", "image/bmp", ""},
		{"TIFF", "sample.tiff", "image/tiff", ""},
		{"ICO", "sample.ico", "image/x-icon", ""},
		{"SVG", "sample.svg", "image/svg+xml", "rsvg-convert"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dependency != "" {
				if _, err := exec.LookPath(tc.dependency); err != nil {
					t.Skipf("%s unavailable", tc.dependency)
				}
			}
			data, err := os.ReadFile(filepath.Join("..", "imaging", "testdata", tc.filename))
			if err != nil {
				t.Fatal(err)
			}
			store := &imagePreviewStore{input: data}
			w := &Worker{Objects: store}
			state, key := w.makePreview(t.Context(), job{AttachmentID: "example", ObjectKey: "original", ContentType: tc.mediaType})
			if state != "ready" || !strings.HasSuffix(key, ".png") || store.mediaType != "image/png" || store.key != key {
				t.Fatalf("state=%q key=%q content-type=%q", state, key, store.mediaType)
			}
			config, err := png.DecodeConfig(bytes.NewReader(store.output))
			if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 640 || config.Height > 640 {
				t.Fatalf("invalid PNG preview: dimensions=%+v error=%v", config, err)
			}
		})
	}
}

func TestImagePreviewRejectsMismatchedContentType(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "imaging", "testdata", "sample.png"))
	if err != nil {
		t.Fatal(err)
	}
	store := &imagePreviewStore{input: data}
	state, _ := (&Worker{Objects: store}).makePreview(t.Context(), job{AttachmentID: "example", ObjectKey: "original", ContentType: "image/heic"})
	if state != "failed" || len(store.output) != 0 {
		t.Fatalf("spoofed image accepted: state=%q size=%d", state, len(store.output))
	}
}

// Keep private camera originals outside the repository while allowing the same
// worker pipeline to be exercised against a reported HEIC decoding failure.
func TestCameraHEICPreview(t *testing.T) {
	filename := os.Getenv("TEST_HEIC_FILE")
	if filename == "" {
		t.Skip("set TEST_HEIC_FILE to test a camera HEIC original")
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	store := &imagePreviewStore{input: data}
	state, key := (&Worker{Objects: store}).makePreview(t.Context(), job{AttachmentID: "camera", ObjectKey: "original", ContentType: "image/heic"})
	if state != "ready" || key != store.key || store.mediaType != "image/png" {
		t.Fatalf("camera HEIC: state=%q key=%q content-type=%q", state, key, store.mediaType)
	}
	config, err := png.DecodeConfig(bytes.NewReader(store.output))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 640 || config.Height > 640 {
		t.Fatalf("invalid camera preview: dimensions=%+v error=%v", config, err)
	}
}
