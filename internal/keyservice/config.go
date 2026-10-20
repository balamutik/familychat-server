// Package keyservice configures direct client access to an independent key server.
// It registers device grants and never downloads content private keys.
package keyservice

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
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

	httpClient *http.Client
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
		return errors.New("invalid key-service registration credentials")
	}
	return nil
}

func (c *Config) PublicKey() *contentcrypto.Key {
	public, _ := base64.StdEncoding.DecodeString(c.Public)
	return &contentcrypto.Key{ID: c.KeyID, Public: public}
}

// DeviceGrant authorizes one direct key delivery to a device proving possession
// of its P-256 private key. It contains no content private key or client session.
type DeviceGrant struct {
	Version         int    `json:"version"`
	GrantID         string `json:"grant_id"`
	UserID          string `json:"user_id"`
	DeviceID        string `json:"device_id"`
	DevicePublicKey string `json:"device_public_key"`
	KeyID           string `json:"key_id"`
	ExpiresAt       int64  `json:"expires_at"`
}

// WithHTTPClient returns a copy using the supplied client's transport for custom
// TLS trust. Grant always applies its own timeout and disables redirects.
func (c *Config) WithHTTPClient(client *http.Client) *Config {
	copy := *c
	copy.httpClient = client
	return &copy
}

func canonicalUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, b := range []byte(value) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !(b >= '0' && b <= '9') && !(b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}

// ValidateDevice requires canonical identifiers and the standard base64 encoding
// of an uncompressed P-256 public point.
func ValidateDevice(deviceID, devicePublicKey string) error {
	if !canonicalUUID(deviceID) {
		return errors.New("invalid device ID")
	}
	public, err := base64.StdEncoding.Strict().DecodeString(devicePublicKey)
	if err != nil || len(public) != 65 || public[0] != 4 || base64.StdEncoding.EncodeToString(public) != devicePublicKey {
		return errors.New("invalid device public key")
	}
	if _, err := ecdh.P256().NewPublicKey(public); err != nil {
		return errors.New("invalid device public key")
	}
	return nil
}

// Grant registers an independently random, short-lived authorization before
// returning it. Only the configured registration secret reaches the key server.
func (c *Config) Grant(ctx context.Context, userID, deviceID, devicePublicKey string, now time.Time) (DeviceGrant, error) {
	var empty DeviceGrant
	if !canonicalUUID(userID) {
		return empty, errors.New("grant requires an authenticated user")
	}
	if err := ValidateDevice(deviceID, devicePublicKey); err != nil {
		return empty, err
	}
	if err := c.Validate(); err != nil {
		return empty, err
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return empty, errors.New("cannot generate device grant")
	}
	grant := DeviceGrant{Version: 1, GrantID: hex.EncodeToString(random), UserID: userID, DeviceID: deviceID, DevicePublicKey: devicePublicKey, KeyID: c.KeyID, ExpiresAt: now.Add(120 * time.Second).Unix()}
	payload, err := json.Marshal(grant)
	if err != nil {
		return empty, errors.New("cannot encode device grant")
	}
	endpoint, err := url.Parse(c.URL)
	if err != nil {
		return empty, errors.New("invalid key-server URL")
	}
	endpoint.Path = "/api/v1/device-grants"
	endpoint.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return empty, errors.New("cannot create device grant request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.Secret)
	request.Header.Set("X-FamilyChat-Issuer", c.Issuer)
	client := http.Client{}
	if c.httpClient != nil {
		client = *c.httpClient
	}
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return empty, errors.New("key-server registration failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(body) > 8192 || response.StatusCode != http.StatusNoContent {
		return empty, errors.New("key-server rejected device grant")
	}
	return grant, nil
}
