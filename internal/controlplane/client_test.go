package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func staticToken(tok string) TokenSource {
	return tokenFunc(func() (string, error) { return tok, nil })
}

type tokenFunc func() (string, error)

func (f tokenFunc) Token() (string, error) { return f() }

func newTestClient(url string, ts TokenSource) *Client {
	return NewClient(func() string { return url }, ts)
}

func TestRegisterEncodesRequestAndAuth(t *testing.T) {
	var captured map[string]any
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/destinations" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		w.Write([]byte(`{"device_id":"11111111-2222-3333-4444-555555555555","heartbeat_interval_seconds":45,"ttl_seconds":90}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, staticToken("tok-abc"))
	resp, err := c.Register(context.Background(), RegisterRequest{
		TunnelURL:          "https://abc.trycloudflare.com",
		DeviceName:         "test-node",
		ModelName:          "llama3.2:3b",
		Capabilities:       []string{"llm_inference"},
		DeviceID:           "99999999-8888-7777-6666-555555555555",
		CapabilitiesStatus: map[string]string{"llm_inference": "live"},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !strings.HasPrefix(gotUA, "silo-node/") {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if captured["tunnel_url"] != "https://abc.trycloudflare.com" ||
		captured["device_name"] != "test-node" ||
		captured["model_name"] != "llama3.2:3b" ||
		captured["device_id"] != "99999999-8888-7777-6666-555555555555" {
		t.Errorf("request body = %v", captured)
	}
	if status, ok := captured["capabilities_status"].(map[string]any); !ok || status["llm_inference"] != "live" {
		t.Errorf("capabilities_status = %v", captured["capabilities_status"])
	}
	if resp.DeviceID != "11111111-2222-3333-4444-555555555555" || resp.HeartbeatIntervalSeconds != 45 || resp.TTLSeconds != 90 {
		t.Errorf("response = %+v", resp)
	}
}

func TestRegisterRetriesWithoutCapabilitiesStatusOn422(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if _, has := body["capabilities_status"]; has {
			http.Error(w, `{"detail":"extra fields not permitted"}`, http.StatusUnprocessableEntity)
			return
		}
		w.Write([]byte(`{"device_id":"11111111-2222-3333-4444-555555555555","heartbeat_interval_seconds":30,"ttl_seconds":90}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, staticToken("tok"))
	resp, err := c.Register(context.Background(), RegisterRequest{
		TunnelURL:          "https://abc.trycloudflare.com",
		Capabilities:       []string{"llm_inference"},
		CapabilitiesStatus: map[string]string{"llm_inference": "live"},
	})
	if err != nil {
		t.Fatalf("Register should succeed via fallback: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(bodies))
	}
	if resp.HeartbeatIntervalSeconds != 30 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestHeartbeatBodyEncoding(t *testing.T) {
	var gotPath, gotContentType string
	var gotBody map[string]map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"ok":true,"ttl_seconds":90}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, staticToken("tok"))
	err := c.Heartbeat(context.Background(), "dev-1", map[string]string{
		"llm_inference": "live",
		"silo_recall":   "dead",
		"silo_remember": "dead",
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if gotPath != "/api/v1/destinations/dev-1/heartbeat" {
		t.Errorf("path = %q", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	want := map[string]string{"llm_inference": "live", "silo_recall": "dead", "silo_remember": "dead"}
	got := gotBody["capabilities_status"]
	if len(got) != len(want) {
		t.Fatalf("capabilities_status = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("capabilities_status[%s] = %q, want %q", k, got[k], v)
		}
	}
}

func TestHeartbeatEmptyStatusSendsNoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Errorf("expected empty body, got %q", body)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL, staticToken("tok")).Heartbeat(context.Background(), "dev-1", nil); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
}

func TestHeartbeatFallsBackToBareOn422(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		body, _ := io.ReadAll(r.Body)
		if len(body) > 0 {
			http.Error(w, `{"detail":"unexpected body"}`, http.StatusUnprocessableEntity)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	err := newTestClient(srv.URL, staticToken("tok")).Heartbeat(
		context.Background(), "dev-1", map[string]string{"llm_inference": "live"})
	if err != nil {
		t.Fatalf("Heartbeat should fall back on 422: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

func TestDelete404IsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		http.Error(w, `{"detail":"Destination not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL, staticToken("tok")).Delete(context.Background(), "dev-1"); err != nil {
		t.Fatalf("Delete on 404 should be nil, got %v", err)
	}
}

func TestDoSurfacesTokenError(t *testing.T) {
	c := newTestClient("http://127.0.0.1:1", &JWTStore{})
	err := c.Heartbeat(context.Background(), "dev-1", nil)
	if err == nil || !strings.Contains(err.Error(), "no control-plane auth token") {
		t.Fatalf("expected ErrNoToken, got %v", err)
	}
}

func TestAPIErrorStatusHelpers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := newTestClient(srv.URL, staticToken("tok")).Heartbeat(context.Background(), "d", nil)
	if !IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("IsStatus(401) = false for %v", err)
	}
	if IsStatus(err, http.StatusNotFound) {
		t.Fatal("IsStatus should not match other codes")
	}
}

func TestLoadOrCreateDeviceID(t *testing.T) {
	dir := t.TempDir()
	id1, err := LoadOrCreateDeviceID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(id1); err != nil {
		t.Fatalf("minted id %q is not a UUID", id1)
	}
	id2, err := LoadOrCreateDeviceID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("device id not stable: %q != %q", id1, id2)
	}
	info, err := os.Stat(filepath.Join(dir, "device_id"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("device_id mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrCreateDeviceIDReplacesCorrupt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "device_id"), []byte("not-a-uuid"), 0o600)
	id, err := LoadOrCreateDeviceID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("expected fresh UUID, got %q", id)
	}
}
