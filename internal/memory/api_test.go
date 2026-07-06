package memory

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testAPI(t *testing.T) (*Capability, http.Handler) {
	t.Helper()
	c := testCapability(t, offSource)
	return c, c.Handler(func() string { return "test-node-key" })
}

func doJSON(t *testing.T, h http.Handler, method, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if key != "" {
		req.Header.Set(NodeKeyHeader, key)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestAPIAuth(t *testing.T) {
	_, h := testAPI(t)

	// Missing key → 401.
	if rr := doJSON(t, h, "GET", "/v1/memory/silos", "", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: %d %s", rr.Code, rr.Body)
	}
	// Wrong key → 401.
	if rr := doJSON(t, h, "GET", "/v1/memory/silos", "wrong", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d", rr.Code)
	}
	// Correct key → 200.
	if rr := doJSON(t, h, "GET", "/v1/memory/silos", "test-node-key", nil); rr.Code != http.StatusOK {
		t.Fatalf("correct key: %d %s", rr.Code, rr.Body)
	}

	// Empty configured key fails closed even with an empty header.
	c := testCapability(t, offSource)
	closedH := c.Handler(func() string { return "" })
	if rr := doJSON(t, closedH, "GET", "/v1/memory/silos", "", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("empty configured key must fail closed: %d", rr.Code)
	}
}

func TestAPIRememberRecallDeleteFlow(t *testing.T) {
	_, h := testAPI(t)
	key := "test-node-key"

	// Remember with metadata.
	rr := doJSON(t, h, "POST", "/v1/memory/silo_a/remember", key,
		map[string]any{"content": "the deploy runs at 9am", "metadata": map[string]any{"tag": "ops"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("remember: %d %s", rr.Code, rr.Body)
	}
	var remembered struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rr.Body.Bytes(), &remembered)
	if remembered.ID == "" {
		t.Fatalf("no id returned: %s", rr.Body)
	}

	// Bad remember body → 400.
	if rr := doJSON(t, h, "POST", "/v1/memory/silo_a/remember", key, map[string]any{"content": ""}); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty content: %d", rr.Code)
	}

	// Recall finds it.
	rr = doJSON(t, h, "POST", "/v1/memory/silo_a/recall", key, map[string]any{"query": "deploy", "limit": 5})
	if rr.Code != http.StatusOK {
		t.Fatalf("recall: %d %s", rr.Code, rr.Body)
	}
	var recalled struct {
		Results []RecallResult `json:"results"`
	}
	json.Unmarshal(rr.Body.Bytes(), &recalled)
	if len(recalled.Results) != 1 || recalled.Results[0].ID != remembered.ID {
		t.Fatalf("recall results: %s", rr.Body)
	}
	if recalled.Results[0].Content != "the deploy runs at 9am" || recalled.Results[0].Metadata["tag"] != "ops" {
		t.Fatalf("recall payload: %+v", recalled.Results[0])
	}

	// Recall in another silo is empty (and an array, not null).
	rr = doJSON(t, h, "POST", "/v1/memory/silo_b/recall", key, map[string]any{"query": "deploy"})
	if !strings.Contains(rr.Body.String(), `"results":[]`) {
		t.Fatalf("cross-silo recall: %s", rr.Body)
	}

	// Silos listing.
	rr = doJSON(t, h, "GET", "/v1/memory/silos", key, nil)
	var silos []SiloCount
	json.Unmarshal(rr.Body.Bytes(), &silos)
	if len(silos) != 1 || silos[0].SiloID != "silo_a" || silos[0].Count != 1 {
		t.Fatalf("silos: %s", rr.Body)
	}

	// Delete.
	if rr := doJSON(t, h, "DELETE", "/v1/memory/silo_a/"+remembered.ID, key, nil); rr.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body)
	}
	if rr := doJSON(t, h, "DELETE", "/v1/memory/silo_a/"+remembered.ID, key, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d", rr.Code)
	}
}

func TestAPINotRunningIs503(t *testing.T) {
	c, _ := testAPI(t)
	h := c.Handler(func() string { return "k" })
	c.Stop(t.Context())
	if rr := doJSON(t, h, "GET", "/v1/memory/silos", "k", nil); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("stopped capability: %d %s", rr.Code, rr.Body)
	}
}

func TestLoadOrCreateNodeKey(t *testing.T) {
	dir := t.TempDir()
	key1, err := LoadOrCreateNodeKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(key1) != 64 {
		t.Fatalf("key length = %d, want 64 hex chars", len(key1))
	}
	info, err := os.Stat(filepath.Join(dir, "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("node.key mode = %v, want 0600", info.Mode().Perm())
	}
	// Stable across restarts.
	key2, err := LoadOrCreateNodeKey(dir)
	if err != nil || key2 != key1 {
		t.Fatalf("key not stable: %q vs %q (%v)", key1, key2, err)
	}
}
