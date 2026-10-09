package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr     string
	DatabaseURL    string
	S3Endpoint     string
	S3Region       string
	S3Bucket       string
	S3AccessKey    string
	S3SecretKey    string
	S3UsePathStyle bool
	TurnURL        string
	TurnSecret     string
	APNsKeyFile    string
	APNsKeyID      string
	APNsTeamID     string
	APNsTopic      string
	APNsSandbox    bool
	AllowedOrigins []string
	SessionTTL     time.Duration
	MaxFileBytes   int64
	UserQuotaBytes int64
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:     value("LISTEN_ADDR", ":8080"),
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		S3Endpoint:     os.Getenv("S3_ENDPOINT"),
		S3Region:       value("S3_REGION", "us-east-1"),
		S3Bucket:       os.Getenv("S3_BUCKET"),
		S3AccessKey:    os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:    os.Getenv("S3_SECRET_KEY"),
		TurnURL:        os.Getenv("TURN_URL"),
		TurnSecret:     os.Getenv("TURN_SECRET"),
		APNsKeyFile:    os.Getenv("APNS_KEY_FILE"),
		APNsKeyID:      os.Getenv("APNS_KEY_ID"),
		APNsTeamID:     os.Getenv("APNS_TEAM_ID"),
		APNsTopic:      os.Getenv("APNS_TOPIC"),
		SessionTTL:     30 * 24 * time.Hour,
		MaxFileBytes:   250 << 20,
		UserQuotaBytes: 10 << 30,
	}
	if v := os.Getenv("SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, errors.New("SESSION_TTL must be a positive duration")
		}
		c.SessionTTL = d
	}
	for _, item := range []struct {
		key string
		dst *int64
	}{{"MAX_FILE_BYTES", &c.MaxFileBytes}, {"USER_QUOTA_BYTES", &c.UserQuotaBytes}} {
		if v := os.Getenv(item.key); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return c, fmt.Errorf("%s must be a positive integer", item.key)
			}
			*item.dst = n
		}
	}
	if c.MaxFileBytes < 1<<20 {
		return c, errors.New("MAX_FILE_BYTES must be at least 1048576")
	}
	if v := os.Getenv("S3_USE_PATH_STYLE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, errors.New("S3_USE_PATH_STYLE must be boolean")
		}
		c.S3UsePathStyle = b
	}
	if v := os.Getenv("APNS_SANDBOX"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, errors.New("APNS_SANDBOX must be boolean")
		}
		c.APNsSandbox = b
	}
	configured := 0
	for _, v := range []string{c.APNsKeyFile, c.APNsKeyID, c.APNsTeamID, c.APNsTopic} {
		if v != "" {
			configured++
		}
	}
	if configured != 0 && configured != 4 {
		return c, errors.New("APNS_KEY_FILE, APNS_KEY_ID, APNS_TEAM_ID and APNS_TOPIC must be set together")
	}
	for _, origin := range strings.Split(os.Getenv("ALLOWED_ORIGINS"), ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			c.AllowedOrigins = append(c.AllowedOrigins, origin)
		}
	}
	for _, kv := range []struct{ name, value string }{{"DATABASE_URL", c.DatabaseURL}, {"S3_BUCKET", c.S3Bucket}, {"S3_ACCESS_KEY", c.S3AccessKey}, {"S3_SECRET_KEY", c.S3SecretKey}, {"TURN_SECRET", c.TurnSecret}, {"TURN_URL", c.TurnURL}} {
		if kv.value == "" {
			return c, fmt.Errorf("%s is required", kv.name)
		}
	}
	if c.UserQuotaBytes < c.MaxFileBytes {
		return c, errors.New("USER_QUOTA_BYTES must be at least MAX_FILE_BYTES")
	}
	return c, nil
}

func value(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
