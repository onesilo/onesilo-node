package openaiapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onesilo/silo-node/internal/config"
)

// ---------------------------------------------------------------------------
// Key store
// ---------------------------------------------------------------------------

func TestKeyStoreMintVerifyRevoke(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadKeyStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	plaintext, key, err := s.Mint("cursor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plaintext, keyPrefix) {
		t.Fatalf("plaintext %q missing prefix", plaintext)
	}
	if key.Last4 != plaintext[len(plaintext)-4:] {
		t.Fatalf("last4 mismatch")
	}
	if !s.Verify(plaintext) {
		t.Fatal("minted key must verify")
	}
	if s.Verify(keyPrefix + strings.Repeat("0", 48)) {
		t.Fatal("unknown key must not verify")
	}
	if s.Verify("") || s.Verify("Bearer nonsense") {
		t.Fatal("non-key strings must not verify")
	}

	// Persistence round-trip: a fresh store from the same dir verifies.
	s2, err := LoadKeyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Verify(plaintext) {
		t.Fatal("key must survive reload")
	}

	if err := s2.Revoke(key.ID); err != nil {
		t.Fatal(err)
	}
	if s2.Verify(plaintext) {
		t.Fatal("revoked key must not verify")
	}
	if err := s2.Revoke(key.ID); err != ErrKeyNotFound {
		t.Fatalf("second revoke: want ErrKeyNotFound, got %v", err)
	}
}

func TestKeyStoreNeverPersistsPlaintext(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadKeyStore(dir)
	plaintext, _, err := s.Mint("leaktest")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, keysFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), plaintext) {
		t.Fatal("plaintext key found in store file")
	}
	// The stored secret material is only the hash.
	var entries []Key
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(entries[0].SHA256) != 64 {
		t.Fatalf("unexpected store contents: %s", raw)
	}
}

func TestKeyStoreCorruptFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, keysFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyStore(dir); err == nil {
		t.Fatal("corrupt store must be an error")
	}
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

func testCapability(t *testing.T, backend string, enabled bool) (*Capability, string) {
	t.Helper()
	keys, err := LoadKeyStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := keys.Mint("test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.OpenAI.Enabled = enabled
	cfg.Capabilities.Compute = true
	if backend != "" {
		cfg.Ollama.Host = backend
	}
	cap := New(func() config.Config { return cfg }, keys, slog.New(slog.DiscardHandler))
	if err := cap.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return cap, plaintext
}

func TestHandlerDisabledIs404(t *testing.T) {
	cap, key := testCapability(t, "", false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	cap.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled surface: want 404, got %d", rec.Code)
	}
}

func TestHandlerRequiresValidKey(t *testing.T) {
	cap, _ := testCapability(t, "", true)
	for _, header := range []string{"", "Bearer wrong", "Bearer " + keyPrefix + strings.Repeat("a", 48)} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		cap.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("header %q: want 401, got %d", header, rec.Code)
		}
		var body struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Type != "authentication_error" {
			t.Fatalf("header %q: expected OpenAI-shaped auth error, got %s", header, rec.Body.String())
		}
	}
}

func TestHandlerProxiesToBackendWithoutAuthorization(t *testing.T) {
	var gotPath, gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"llama3.2:3b"}]}`)
	}))
	defer backend.Close()

	cap, key := testCapability(t, backend.URL, true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	cap.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/models" {
		t.Fatalf("backend saw path %q", gotPath)
	}
	if gotAuth != "" {
		t.Fatalf("node API key leaked to backend: %q", gotAuth)
	}
	if !strings.Contains(rec.Body.String(), "llama3.2:3b") {
		t.Fatalf("backend body not passed through: %s", rec.Body.String())
	}
}

func TestHandlerStreamsSSEIncrementally(t *testing.T) {
	firstChunkSent := make(chan struct{})
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"chunk\":1}\n\n")
		flusher.Flush()
		close(firstChunkSent)
		<-release
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer backend.Close()

	cap, key := testCapability(t, backend.URL, true)
	front := httptest.NewServer(cap.Handler())
	defer front.Close()

	req, _ := http.NewRequest("POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	<-firstChunkSent
	// The first SSE event must be readable before the backend finishes —
	// i.e. the proxy flushes instead of buffering the whole response.
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading first SSE line: %v", err)
	}
	if !strings.Contains(line, `{"chunk":1}`) {
		t.Fatalf("first streamed line = %q", line)
	}
	close(release)
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerBadBackendIs502(t *testing.T) {
	cap, key := testCapability(t, "http://127.0.0.1:1", true) // nothing listens
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	cap.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
}
