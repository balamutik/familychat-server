// Package keyservice configures direct client access to an independent key server.
// It intentionally has no HTTP client and never downloads content private keys.
package keyservice

import (
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"familychat/server/internal/contentcrypto"
)

type Config struct {
	Scheme string `json:"scheme"`
	KeyID  string `json:"key_id"`
	Public string `json:"public_key"`
	URL    string `json:"key_server_url"`
	Issuer string `json:"ticket_issuer"`
	Secret string `json:"ticket_secret"`
}

func Load(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, errors.New("cannot read key-service integration file")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16384 {
		return nil, errors.New("key-service integration must be a restricted regular file (0400 or 0600)")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open key-service integration file")
	}
	defer file.Close()
	var config Config
	decoder := json.NewDecoder(io.LimitReader(file, 16385))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, errors.New("invalid key-service integration JSON")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("invalid key-service integration JSON")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

func (c *Config) Validate() error {
	public, err := base64.StdEncoding.Strict().DecodeString(c.Public)
	if err != nil || len(public) != 32 || c.Scheme != "fc1" {
		return errors.New("invalid fc1 public key")
	}
	if _, err = ecdh.X25519().NewPublicKey(public); err != nil {
		return errors.New("invalid public key")
	}
	id := sha256.Sum256(public)
	if c.KeyID != hex.EncodeToString(id[:]) {
		return errors.New("public key fingerprint does not match key_id")
	}
	endpoint, err := url.Parse(c.URL)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "/api/v1/keys/current" {
		return errors.New("invalid key-server URL")
	}
	loopback := endpoint.Hostname() == "localhost" || net.ParseIP(endpoint.Hostname()).IsLoopback()
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && loopback) {
		return errors.New("key-server URL requires HTTPS")
	}
	port, portErr := strconv.Atoi(endpoint.Port())
	if portErr != nil || port < 1 || port > 65535 || port == 80 || port == 443 || port == 8080 {
		return errors.New("key-server URL requires a separate explicit port (default 8443)")
	}
	secret, err := base64.StdEncoding.Strict().DecodeString(c.Secret)
	if err != nil || len(secret) != 32 || strings.TrimSpace(c.Issuer) == "" || len(c.Issuer) > 200 {
		return errors.New("invalid key-service ticket credentials")
	}
	return nil
}

func (c *Config) PublicKey() *contentcrypto.Key {
	public, _ := base64.StdEncoding.DecodeString(c.Public)
	return &contentcrypto.Key{ID: c.KeyID, Public: public}
}

func (c *Config) Ticket(userID string, now time.Time) (string, time.Time, error) {
	if userID == "" {
		return "", time.Time{}, errors.New("ticket requires an authenticated user")
	}
	secret, err := base64.StdEncoding.DecodeString(c.Secret)
	if err != nil || len(secret) != 32 {
		return "", time.Time{}, errors.New("invalid ticket secret")
	}
	expires := now.Add(120 * time.Second)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(struct {
		Issuer   string `json:"iss"`
		Audience string `json:"aud"`
		Subject  string `json:"sub"`
		KeyID    string `json:"key_id"`
		Issued   int64  `json:"iat"`
		Expires  int64  `json:"exp"`
	}{c.Issuer, "familychat-keyserver", userID, c.KeyID, now.Unix(), expires.Unix()})
	if err != nil {
		return "", time.Time{}, err
	}
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), expires, nil
}
