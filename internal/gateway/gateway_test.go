package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onesilo/silo-node/internal/config"
	"github.com/onesilo/silo-node/internal/memory"
)

type staticTokens struct {
	token string
	err   error
}

func (s staticTokens) Token() (string, error) { return s.token, s.err }

func testCapability(t *testing.T, controlPlaneURL string, tokens staticTokens) *Capability {
	t.Helper()
	cfg := config.Default()
	cfg.Mode = config.ModeGateway
	cfg.ControlPlane.URL = controlPlaneURL
	c := New(func() config.Config { return cfg }, tokens, slog.New(slog.DiscardHandler))
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	return c
}

const nodeKey = "test-node-key"

func relay(t *testing.T, c *Capability, method, path string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c.Handler(func() string { return nodeKey }).ServeHTTP(rec, req)
	return rec
}

func withKey() map[string]string {
	return map[string]string{memory.NodeKeyHeader: nodeKey}
}

func TestRelayForwardsWithCredentials(t *testing.T) {
	var got *http.Request
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"silos": []}`))
	}))
	defer backend.Close()

	c := testCapability(t, backend.URL, staticTokens{token: "sc_secret"})
	rec := relay(t, c, http.MethodGet, "/v1/cloud/api/v1/silos?page=2", withKey())

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
	if got == nil {
		t.Fatal("backend was never called")
	}
	if got.URL.Path != "/api/v1/silos" || got.URL.RawQuery != "page=2" {
		t.Fatalf("unexpected forwarded URL %q?%q", got.URL.Path, got.URL.RawQuery)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer sc_secret" {
		t.Fatalf("expected the node's bearer token, got %q", auth)
	}
	if got.Header.Get(memory.NodeKeyHeader) != "" {
		t.Fatal("the node key must not leak to the control plane")
	}
}

func TestRelayRequiresNodeKey(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("backend must not be reached without the node key")
	}))
	defer backend.Close()

	c := testCapability(t, backend.URL, staticTokens{token: "sc_secret"})
	if rec := relay(t, c, http.MethodGet, "/v1/cloud/api/v1/silos", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without node key, got %d", rec.Code)
	}
	wrong := map[string]string{memory.NodeKeyHeader: "wrong"}
	if rec := relay(t, c, http.MethodGet, "/v1/cloud/api/v1/silos", wrong); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with a wrong node key, got %d", rec.Code)
	}
}

func TestRelayAllowlist(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	c := testCapability(t, backend.URL, staticTokens{token: "sc_secret"})

	allowed := []string{"/v1/cloud/api/v1/silos", "/v1/cloud/mcp", "/v1/cloud/mcp/session"}
	for _, p := range allowed {
		if rec := relay(t, c, http.MethodGet, p, withKey()); rec.Code == http.StatusForbidden {
			t.Errorf("%s should be relayed, got 403", p)
		}
	}
	blocked := []string{"/v1/cloud/oauth/register", "/v1/cloud/admin/users", "/v1/cloud/mcpx", "/v1/cloud/apifoo"}
	for _, p := range blocked {
		if rec := relay(t, c, http.MethodGet, p, withKey()); rec.Code != http.StatusForbidden {
			t.Errorf("%s must not be relayed, got %d", p, rec.Code)
		}
	}
}

func TestRelayWithoutCredentialsIs503(t *testing.T) {
	c := testCapability(t, "http://127.0.0.1:0", staticTokens{err: errors.New("no token")})
	rec := relay(t, c, http.MethodGet, "/v1/cloud/api/v1/silos", withKey())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without credentials, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !strings.Contains(body["error"], "credentials") {
		t.Fatalf("expected a credentials error body, got %s", rec.Body)
	}
}

func TestRelayUnreachableControlPlaneIs502(t *testing.T) {
	// A closed port: connection refused.
	c := testCapability(t, "http://127.0.0.1:1", staticTokens{token: "sc_secret"})
	rec := relay(t, c, http.MethodGet, "/v1/cloud/api/v1/silos", withKey())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for an unreachable control plane, got %d", rec.Code)
	}
}

func TestRelayStoppedIs503(t *testing.T) {
	c := testCapability(t, "http://127.0.0.1:1", staticTokens{token: "sc_secret"})
	_ = c.Stop(context.Background())
	rec := relay(t, c, http.MethodGet, "/v1/cloud/api/v1/silos", withKey())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when stopped, got %d", rec.Code)
	}
}

func TestEnabledFollowsMode(t *testing.T) {
	cfg := config.Default()
	c := New(func() config.Config { return cfg }, staticTokens{}, slog.New(slog.DiscardHandler))
	if c.Enabled() {
		t.Fatal("gateway must be disabled in local mode")
	}
	cfg.Mode = config.ModeGateway
	if !c.Enabled() {
		t.Fatal("gateway must be enabled in gateway mode")
	}
}
