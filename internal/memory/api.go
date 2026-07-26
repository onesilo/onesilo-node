package memory

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// NodeKeyHeader authenticates memory API requests. The key is a node-local
// secret (data_dir/node.key), surfaced to the desktop app / control plane
// via the admin API's GET /v1/status — deliberately NOT the LAN pairing key
// (a transport secret must never become an authorization credential).
const NodeKeyHeader = "X-Silo-Node-Key"

// maxBodyBytes bounds remember/recall request bodies. Memory content is
// prose, not payloads; a few MB is generous and stops a node-key holder
// from exhausting node memory with an unbounded body.
const maxBodyBytes = 4 << 20 // 4 MiB

// maxRecallLimit caps how many hits a single recall may request, bounding
// the over-fetch + per-hit decryption amplification.
const maxRecallLimit = 100

// LoadOrCreateNodeKey returns the node API key from <dataDir>/node.key,
// generating a 32-byte random hex token (0600) on first start.
func LoadOrCreateNodeKey(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "node.key")
	raw, err := os.ReadFile(path)
	if err == nil {
		if key := strings.TrimSpace(string(raw)); key != "" {
			return key, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("reading node key: %w", err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating node key: %w", err)
	}
	key := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return "", fmt.Errorf("writing node key: %w", err)
	}
	return key, nil
}

// Handler returns the memory HTTP API, mounted on the LAN server:
//
//	POST   /v1/memory/{silo_id}/remember     {content, metadata?} → {id}
//	POST   /v1/memory/{silo_id}/recall       {query, limit?}      → {results}
//	GET    /v1/memory/silos                                       → [{silo_id, count}]
//	DELETE /v1/memory/{silo_id}/{memory_id}
//
// Every route requires the X-Silo-Node-Key header. nodeKey is resolved per
// request so key rotation needs no restart.
func (c *Capability) Handler(nodeKey func() string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/memory/silos", func(w http.ResponseWriter, r *http.Request) {
		silos, err := c.Silos(r.Context())
		if err != nil {
			writeMemErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, silos)
	})

	mux.HandleFunc("POST /v1/memory/{silo_id}/remember", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content  string         `json:"content"`
			Metadata map[string]any `json:"metadata"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Content) == "" {
			writeError(w, http.StatusBadRequest, "expected JSON body {\"content\": \"...\", \"metadata\": {...}?}")
			return
		}
		id, err := c.Remember(r.Context(), r.PathValue("silo_id"), body.Content, body.Metadata)
		if err != nil {
			writeMemErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	})

	mux.HandleFunc("POST /v1/memory/{silo_id}/recall", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Query) == "" {
			writeError(w, http.StatusBadRequest, "expected JSON body {\"query\": \"...\", \"limit\": n?}")
			return
		}
		if body.Limit > maxRecallLimit {
			body.Limit = maxRecallLimit
		}
		results, err := c.Recall(r.Context(), r.PathValue("silo_id"), body.Query, body.Limit)
		if err != nil {
			writeMemErr(w, err)
			return
		}
		if results == nil {
			results = []RecallResult{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	})

	mux.HandleFunc("DELETE /v1/memory/{silo_id}/{memory_id}", func(w http.ResponseWriter, r *http.Request) {
		deleted, err := c.Forget(r.Context(), r.PathValue("silo_id"), r.PathValue("memory_id"))
		if err != nil {
			writeMemErr(w, err)
			return
		}
		if !deleted {
			writeError(w, http.StatusNotFound, "memory not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	})

	return RequireNodeKey(nodeKey, mux)
}

// RequireNodeKey rejects requests without the correct X-Silo-Node-Key.
// Fails closed when no key is configured. Shared with every other
// node-key-authenticated surface (e.g. the gateway relay).
func RequireNodeKey(nodeKey func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := nodeKey()
		got := r.Header.Get(NodeKeyHeader)
		if want == "" || got == "" || !constantTimeEqual(want, got) {
			writeError(w, http.StatusUnauthorized, "missing or invalid "+NodeKeyHeader)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// constantTimeEqual compares two secrets without leaking length via timing:
// both are SHA-256'd to a fixed width before the constant-time compare
// (mirrors the admin-token check in internal/adminapi/auth.go).
func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

func writeMemErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotRunning) {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
