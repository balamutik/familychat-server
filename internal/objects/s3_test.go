package objects

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"familychat/server/internal/config"
)

func TestPingRejectsMissingBucket(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set TEST_S3_ENDPOINT to run S3 integration test")
	}
	c := config.Config{
		S3Endpoint: endpoint, S3Region: "us-east-1", S3Bucket: "familychat-test-missing-bucket",
		S3AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"), S3SecretKey: os.Getenv("TEST_S3_SECRET_KEY"),
		S3UsePathStyle: true,
	}
	s := New(c)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Ping(ctx); err == nil {
		t.Fatal("missing bucket unexpectedly reported ready")
	}
}

func TestRoundTripAndRange(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set TEST_S3_ENDPOINT")
	}
	s := New(config.Config{S3Endpoint: endpoint, S3Region: "us-east-1", S3Bucket: "familychat-test", S3AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"), S3SecretKey: os.Getenv("TEST_S3_SECRET_KEY"), S3UsePathStyle: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := "test/round-trip-20260929.txt"
	defer s.Delete(context.Background(), key)
	if err := s.Put(ctx, key, strings.NewReader("hello world"), 11, "text/plain"); err != nil {
		t.Fatal(err)
	}
	meta, err := s.Head(ctx, key)
	if err != nil || meta.Size != 11 {
		t.Fatalf("head=%+v err=%v", meta, err)
	}
	obj, err := s.Get(ctx, key, "bytes=6-10")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Body.Close()
	got, err := io.ReadAll(obj.Body)
	if err != nil || string(got) != "world" || obj.ContentRange != "bytes 6-10/11" {
		t.Fatalf("range=%q header=%q err=%v", got, obj.ContentRange, err)
	}
}

func TestPutFromNonSeekableRequestBody(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set TEST_S3_ENDPOINT")
	}
	s := New(config.Config{S3Endpoint: endpoint, S3Region: "us-east-1", S3Bucket: "familychat-test", S3AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"), S3SecretKey: os.Getenv("TEST_S3_SECRET_KEY"), S3UsePathStyle: true})
	key := "test/nonseek-20260929.txt"
	defer s.Delete(context.Background(), key)
	body := io.NopCloser(strings.NewReader("hello world"))
	if err := s.Put(context.Background(), key, body, 11, "text/plain"); err != nil {
		t.Fatalf("non-seekable Put: %v", err)
	}
}
