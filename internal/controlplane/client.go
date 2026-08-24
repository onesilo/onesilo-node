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

	"github.com/onesilo/onesilo-node/internal/fsutil"
	"github.com/onesilo/onesilo-node/internal/version"
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
	// IdentityPubKey is the node's long-term P-256 identity public key
	// (base64 uncompressed point), published so the control plane can attest
	// it in pairing assertions. Omitted when automated pairing is off.
	IdentityPubKey string `json:"device_public_key,omitempty"`
	// InferenceKey is a bearer key for this node's OpenAI-compatible
	// surface, minted for the control plane so it can run inference here on
	// the owner's behalf -- indexing a local_only artifact is the first use
	// (SILO-757/SILO-764). Omitted unless the operator turned on
	// openai.publish_key_to_control_plane; the backend preserves the key it
	// already holds when the field is absent, so omitting it is not a
	// revocation.
	InferenceKey string `json:"inference_key,omitempty"`
}

// RegisterResponse is the subset of the registration echo we use.
type RegisterResponse struct {
	DeviceID                 string `json:"device_id"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	TTLSeconds               int    `json:"ttl_seconds"`
	// AccountID is the owning account the control plane assigned this
	// destination. The node pins it so a pairing assertion for a *different*
	// account is rejected (cross-tenant confused-deputy defense).
	AccountID string `json:"account_id"`
}

// Register registers (or re-registers) this node as a destination.
// If the server rejects a newer field as unknown (422 from a pre-rollout
// deploy), the request is retried without the fields a pre-rollout server
// would not recognize.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	var resp RegisterResponse
	err := c.do(ctx, http.MethodPost, "/api/v1/destinations", req, &resp)
	if err != nil && IsStatus(err, 422) &&
		(req.CapabilitiesStatus != nil || req.InferenceKey != "") {
		// Which of the two the server choked on is not distinguishable from
		// a 422, so drop both. Losing capabilities_status costs liveness
		// detail; losing inference_key means that server is too old to use
		// the key anyway.
		fallback := req
		fallback.CapabilitiesStatus = nil
		fallback.InferenceKey = ""
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

// ProvisionTunnelRequest is the POST /api/v1/nodes/tunnel/provision body.
type ProvisionTunnelRequest struct {
	DeviceID string `json:"device_id"`
	// LocalPort is the localhost port cloudflared fronts on this node; the
	// control plane builds the ingress origin itself.
	LocalPort int `json:"local_port"`
}

// ProvisionTunnelResponse carries the managed tunnel's stable hostname and
// the cloudflared run token for this start. The token is a secret: it is
// used once to spawn cloudflared and never persisted or logged.
type ProvisionTunnelResponse struct {
	Hostname  string `json:"hostname"`
	TunnelURL string `json:"tunnel_url"`
	Token     string `json:"token"`
}

// ProvisionTunnel asks the control plane for this node's managed named
// tunnel (creating it on first call; the hostname is stable afterwards).
// Paid feature: a free account gets a 402 (check with IsStatus).
func (c *Client) ProvisionTunnel(ctx context.Context, deviceID string, localPort int) (*ProvisionTunnelResponse, error) {
	var resp ProvisionTunnelResponse
	err := c.do(ctx, http.MethodPost, "/api/v1/nodes/tunnel/provision",
		ProvisionTunnelRequest{DeviceID: deviceID, LocalPort: localPort}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
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
//
// Written atomically, like the node's other identity files: a crash during a
// plain WriteFile can leave a truncated id, and LoadOrCreateDeviceID treats
// an unparseable file as corrupt and mints a fresh one — which is exactly the
// case the stable id exists to prevent, since the control plane would then
// accumulate a second destination for the same machine.
func SaveDeviceID(dataDir, id string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	path := filepath.Join(dataDir, deviceIDFile)
	if err := fsutil.WriteFileAtomic(path, []byte(id+"\n"), 0o600); err != nil {
		return fmt.Errorf("persisting device id: %w", err)
	}
	return nil
}
