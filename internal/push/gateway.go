package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"familychat/server/internal/config"
	"familychat/server/internal/pushprotocol"
)

// Gateway holds only a credential scoped to this chat installation.
type Gateway struct {
	url, credential string
	http            *http.Client
}

func NewGateway(c config.Config) (*Gateway, error) {
	if err := c.ValidatePush(); err != nil {
		return nil, err
	}
	f, err := os.Open(c.PushGatewayTokenFile)
	if err != nil {
		return nil, errors.New("cannot read push gateway token file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 256 {
		return nil, errors.New("push gateway token must be a private file (0400 or 0600)")
	}
	data, err := io.ReadAll(io.LimitReader(f, 257))
	credential := strings.TrimSpace(string(data))
	if err != nil || !pushprotocol.ValidCredential(credential) {
		return nil, errors.New("invalid push gateway token")
	}
	return &Gateway{url: strings.TrimRight(c.PushGatewayURL, "/"), credential: credential, http: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("push gateway redirect refused") }}}, nil
}
func (g *Gateway) request(ctx context.Context, path string, input any) (int, []byte, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return 0, nil, errors.New("invalid push gateway request")
	}
	r, err := http.NewRequestWithContext(ctx, "POST", g.url+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, errors.New("invalid push gateway URL")
	}
	r.Header.Set("Authorization", "Bearer "+g.credential)
	r.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(r)
	if err != nil {
		return 0, nil, errors.New("push gateway connection failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(body) > 4096 {
		return 0, nil, errors.New("invalid push gateway response")
	}
	return resp.StatusCode, body, nil
}
func (g *Gateway) Send(ctx context.Context, token, kind string, payload json.RawMessage) (bool, error) {
	if len(payload) > pushprotocol.MaxBodyBytes {
		return false, errors.New("push payload too large")
	}
	// Decode only routing fields. Never forward APS content, display names or message text.
	var event pushprotocol.Event
	if json.Unmarshal(payload, &event) != nil {
		return false, errors.New("invalid push event")
	}
	if kind == "voip" && event.ExpiresAt == 0 {
		event.ExpiresAt = time.Now().Add(45 * time.Second).Unix()
	} // Legacy queued jobs; worker checks call freshness.
	if _, err := event.Payload(kind, time.Now()); err != nil {
		return false, err
	}
	deviceKind := pushprotocol.DeviceKind(kind)
	if !pushprotocol.ValidDevice(deviceKind, token) {
		return false, errors.New("invalid push device")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	code, data, err := g.request(ctx, "/v1/devices", pushprotocol.Registration{Kind: deviceKind, Token: token})
	if err != nil {
		return false, err
	}
	if code != 201 {
		return false, fmt.Errorf("push gateway registration HTTP %d", code)
	}
	var registered pushprotocol.RegistrationResult
	if json.Unmarshal(data, &registered) != nil || len(registered.ID) != 32 {
		return false, errors.New("invalid push gateway registration response")
	}
	code, data, err = g.request(ctx, "/v1/push", pushprotocol.Request{DeviceID: registered.ID, Kind: kind, Event: event})
	if err != nil {
		return false, err
	}
	if code == 204 {
		return false, nil
	}
	if code == 410 {
		var failure struct {
			Code string `json:"error"`
		}
		if json.Unmarshal(data, &failure) == nil && failure.Code == "device_unregistered" {
			return true, errors.New("push gateway device unregistered")
		}
	}
	return false, fmt.Errorf("push gateway delivery HTTP %d", code)
}
