package openaiapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/onesilo/onesilo-node/internal/config"
)

// Routes are the OpenAI-compatible paths the capability serves. Exact
// paths (not a /v1/ prefix): /v1/memory/ and /v1/cloud/ live on the same
// mux and must keep their own auth models.
var Routes = []string{
	"/v1/chat/completions",
	"/v1/completions",
	"/v1/models",
	"/v1/embeddings",
}

// Capability is the OpenAI-compatible surface as a node capability: no
// owned process (the LAN server carries the listener and Ollama does the
// inference), so Start/Stop only flip the serving gate. It exists as a
// capability for the admin status row and the reconciler's config watch.
type Capability struct {
	getCfg  func() config.Config
	keys    *KeyStore
	logger  *slog.Logger
	running atomic.Bool
}

// New builds the capability. keys must be non-nil.
func New(getCfg func() config.Config, keys *KeyStore, logger *slog.Logger) *Capability {
	return &Capability{getCfg: getCfg, keys: keys, logger: logger}
}

// Name implements node.Capability.
func (c *Capability) Name() string { return "openai" }

// Enabled implements node.Capability.
func (c *Capability) Enabled() bool {
	cfg := c.getCfg()
	return cfg.OpenAI.Enabled && cfg.Capabilities.Compute
}

// Start implements node.Capability.
func (c *Capability) Start(ctx context.Context) error {
	c.running.Store(true)
	return nil
}

// Stop implements node.Capability.
func (c *Capability) Stop(ctx context.Context) error {
	c.running.Store(false)
	return nil
}

// Healthy implements node.Capability. Inference liveness belongs to the
// compute capability (same Ollama); this reports the surface's own state.
func (c *Capability) Healthy(ctx context.Context) (bool, string) {
	if !c.running.Load() {
		return false, "not running"
	}
	n := c.keys.Count()
	if n == 0 {
		return true, "serving (no API keys minted yet)"
	}
	return true, fmt.Sprintf("serving, %d API key(s)", n)
}

// Keys exposes the key store for the admin API.
func (c *Capability) Keys() *KeyStore { return c.keys }

// Handler returns the HTTP handler to mount at each of Routes on the LAN
// server. Order of gates: surface enabled (404 — the endpoint doesn't
// exist when off), then bearer key auth (401), then reverse-proxy to the
// local Ollama's OpenAI-compatible API with streaming flushes.
func (c *Capability) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := c.getCfg()
		if !cfg.OpenAI.Enabled || !cfg.Capabilities.Compute || !c.running.Load() {
			http.NotFound(w, r)
			return
		}

		presented, ok := bearerToken(r)
		if !ok || !c.keys.Verify(presented) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="onesilo-node"`)
			writeOpenAIError(w, http.StatusUnauthorized,
				"invalid or missing API key; mint one with `onesilo-node` admin API or the desktop app")
			return
		}

		target, err := url.Parse(cfg.Ollama.Host)
		if err != nil || target.Host == "" {
			writeOpenAIError(w, http.StatusBadGateway, "local inference backend is misconfigured")
			return
		}

		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.URL.Path = r.URL.Path // preserve /v1/... verbatim
				// The node key must not travel to the backend, and Ollama
				// needs no credentials on loopback.
				pr.Out.Header.Del("Authorization")
			},
			// Flush every write immediately: SSE token streams must not
			// buffer.
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				c.logger.Warn("openai proxy error", "path", r.URL.Path, "err", err)
				writeOpenAIError(w, http.StatusBadGateway, "local inference backend is unavailable")
			},
		}
		proxy.ServeHTTP(w, r)
	})
}

// bearerToken extracts the Authorization: Bearer credential.
func bearerToken(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	const scheme = "Bearer "
	if len(auth) <= len(scheme) || !strings.EqualFold(auth[:len(scheme)], scheme) {
		return "", false
	}
	return strings.TrimSpace(auth[len(scheme):]), true
}

// writeOpenAIError writes the error shape OpenAI clients know how to
// display: {"error": {"message": ..., "type": ...}}.
func writeOpenAIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	kind := "invalid_request_error"
	if status == http.StatusUnauthorized {
		kind = "authentication_error"
	}
	fmt.Fprintf(w, `{"error":{"message":%q,"type":%q}}`, message, kind)
}
