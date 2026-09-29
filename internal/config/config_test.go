package config

import (
	"strings"
	"testing"
)

func TestConfigRejectsMissingSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("S3_BUCKET", "")
	t.Setenv("S3_ENDPOINT", "")
	t.Setenv("S3_ACCESS_KEY", "")
	t.Setenv("S3_SECRET_KEY", "")
	t.Setenv("TURN_SECRET", "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("Load() error = %v, want missing DATABASE_URL", err)
	}
}
