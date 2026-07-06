package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/onesilo/silo-node/internal/version"
)

// APIError is a non-2xx control-plane response.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("control plane returned %d: %s", e.StatusCode, e.Body)
}

// IsStatus reports whether err is an APIError with the given status code.
func IsStatus(err error, code int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}

// Client calls the destinations API on the Silo control plane.
// Contract (feature/llm-tunnels, app/routers/llm_destinations.py):
//
//	POST   /api/v1/destinations                       register / re-register
//	POST   /api/v1/destinations/{device_id}/heartbeat keep-alive
//	DELETE /api/v1/destinations/{device_id}           remove on shutdown
type Client struct {
	// baseURL is read live so admin-API config changes apply immediately.
	baseURL func() string
	tokens  TokenSource
	http    *http.Client
}

// NewClient builds a client. baseURL is a getter over live config.
func NewClient(baseURL func() string, tokens TokenSource) *Client {
	return &Client{
		baseURL: baseURL,
		tokens:  tokens,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// RegisterRequest is the POST /api/v1/destinations body.
type RegisterRequest struct {
	TunnelURL    string   `json:"tunnel_url"`
	DeviceName   string   `json:"device_name"`
	ModelName    string   `json:"model_name"`
	Capabilities []string `json:"capabilities"`
	// DeviceID is the persisted stable id; the server upserts on it.
	DeviceID string `json:"device_id,omitempty"`
	// CapabilitiesStatus is per-capability liveness ("live"|"dead"). Newer
	// servers accept it; older ones may 422, handled by the caller.
	CapabilitiesStatus map[string]string `json:"capabilities_status,omitempty"`
}

// RegisterResponse is the subset of the registration echo we use.
type RegisterResponse struct {
	DeviceID                 string `json:"device_id"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	TTLSeconds               int    `json:"ttl_seconds"`
}

// Register registers (or re-registers) this node as a destination.
// If the server rejects capabilities_status as an unknown field (422 from a
// pre-rollout deploy), the request is retried without it.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	var resp RegisterResponse
	err := c.do(ctx, http.MethodPost, "/api/v1/destinations", req, &resp)
	if err != nil && IsStatus(err, 422) && req.CapabilitiesStatus != nil {
		fallback := req
		fallback.CapabilitiesStatus = nil
		err = c.do(ctx, http.MethodPost, "/api/v1/destinations", fallback, &resp)
	}
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// heartbeatRequest is the optional heartbeat body.
type heartbeatRequest struct {
	CapabilitiesStatus map[string]string `json:"capabilities_status"`
}

// Heartbeat refreshes the destination's liveness. status may be nil; when
// the server predates the capabilities_status body (422), the heartbeat is
// retried bare so an old backend still keeps us alive.
func (c *Client) Heartbeat(ctx context.Context, deviceID string, status map[string]string) error {
	path := "/api/v1/destinations/" + deviceID + "/heartbeat"
	if len(status) > 0 {
		err := c.do(ctx, http.MethodPost, path, heartbeatRequest{CapabilitiesStatus: status}, nil)
		if err == nil || !IsStatus(err, 422) {
			return err
		}
	}
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// Delete removes this node's registration. A 404 (already gone) is success.
func (c *Client) Delete(ctx context.Context, deviceID string) error {
	err := c.do(ctx, http.MethodDelete, "/api/v1/destinations/"+deviceID, nil, nil)
	if IsStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	token, err := c.tokens.Token()
	if err != nil {
		return err
	}

	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.baseURL(), "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", version.UserAgent())
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &APIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// deviceIDFile is where the stable device id lives under data_dir.
const deviceIDFile = "device_id"

// LoadOrCreateDeviceID returns the persisted device id from
// <dataDir>/device_id, minting and persisting a fresh UUID on first run.
// The id is stable across restarts so quick-tunnel re-registrations upsert
// instead of accumulating stale destinations.
func LoadOrCreateDeviceID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, deviceIDFile)
	if raw, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(raw))
		if _, err := uuid.Parse(id); err == nil {
			return id, nil
		}
		// Corrupt file: fall through and mint a new id.
	}
	id := uuid.NewString()
	if err := SaveDeviceID(dataDir, id); err != nil {
		return "", err
	}
	return id, nil
}

// SaveDeviceID persists the device id (0600).
func SaveDeviceID(dataDir, id string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	path := filepath.Join(dataDir, deviceIDFile)
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return fmt.Errorf("persisting device id: %w", err)
	}
	return nil
}
