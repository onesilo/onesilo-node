// Package gateway is the control-plane relay capability: in gateway mode
// the node exposes the One Silo cloud surface — cloud silos, connectors,
// and the MCP gateway — to local clients over the LAN port, attaching the
// node's own control-plane credentials to every forwarded request. Local
// clients authenticate with the node key (X-Silo-Node-Key) and never hold
// cloud credentials themselves.
//
//	/v1/cloud/api/...  →  <control_plane.url>/api/...
//	/v1/cloud/mcp      →  <control_plane.url>/mcp
//
// Only the API and MCP surfaces are relayed (path allowlist); everything
// else is refused.
package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/onesilo/onesilo-node/internal/config"
	"github.com/onesilo/onesilo-node/internal/controlplane"
	"github.com/onesilo/onesilo-node/internal/memory"
	"github.com/onesilo/onesilo-node/internal/version"
)

// RoutePrefix is where the relay mounts on the LAN server.
const RoutePrefix = "/v1/cloud"

// probeTimeout bounds the control-plane reachability check in Healthy.
const probeTimeout = 3 * time.Second

// noCredentialsHint names every supported credential path, so a user who
// chose OAuth during setup isn't steered toward an API key by mistake.
const noCredentialsHint = "sign in with `silo-node setup`, set " +
	controlplane.APIKeyEnvVar + ", or push a JWT"

// Capability implements node.Capability for the control-plane relay.
type Capability struct {
	getCfg func() config.Config
	tokens controlplane.TokenSource
	logger *slog.Logger

	running atomic.Bool
}

// New builds the gateway capability. tokens is the same modal source the
// registration manager uses: the setup sign-in's OAuth credential,
// SILO_API_KEY, or a JWT pushed by the desktop app.
func New(getCfg func() config.Config, tokens controlplane.TokenSource, logger *slog.Logger) *Capability {
	return &Capability{getCfg: getCfg, tokens: tokens, logger: logger}
}

// Name implements node.Capability.
func (c *Capability) Name() string { return "gateway" }

// Enabled implements node.Capability: the relay runs exactly when the node
// is in gateway mode.
func (c *Capability) Enabled() bool { return c.getCfg().Mode == config.ModeGateway }

// Start implements node.Capability. The relay handler is mounted on the LAN
// server unconditionally; Start only flips the serving switch.
func (c *Capability) Start(ctx context.Context) error {
	c.running.Store(true)
	c.logger.Info("control-plane relay serving", "prefix", RoutePrefix, "target", c.getCfg().ControlPlane.URL)
	return nil
}

// Stop implements node.Capability.
func (c *Capability) Stop(ctx context.Context) error {
	c.running.Store(false)
	c.logger.Info("control-plane relay stopped")
	return nil
}

// Healthy implements node.Capability: running, credentialed, and the
// control plane answers at the transport level.
func (c *Capability) Healthy(ctx context.Context) (bool, string) {
	if !c.running.Load() {
		return false, "not started"
	}
	if _, err := c.tokens.Token(); err != nil {
		return false, "no control-plane credentials (" + noCredentialsHint + ")"
	}
	target := c.getCfg().ControlPlane.URL
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, target, nil)
	if err != nil {
		return false, "invalid control plane URL: " + err.Error()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "control plane unreachable: " + err.Error()
	}
	resp.Body.Close()
	return true, "relaying " + RoutePrefix + " to " + target
}

// proxyStateKey carries the per-request target and token from the outer
// handler into the proxy's Rewrite (which cannot fail on its own).
type proxyStateKey struct{}

type proxyState struct {
	target *url.URL
	token  string
}

// Handler returns the relay HTTP API. Every request requires the node key;
// the path after RoutePrefix must be on the allowlist and is forwarded
// verbatim (with query) to the control plane with this node's bearer token.
// nodeKey is resolved per request so key rotation needs no restart.
func (c *Capability) Handler(nodeKey func() string) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			st := pr.In.Context().Value(proxyStateKey{}).(*proxyState)
			pr.Out.URL = st.target
			pr.Out.Host = st.target.Host
			pr.Out.Header.Del(memory.NodeKeyHeader)
			// Drop client-supplied headers that would otherwise ride upstream
			// under the node's cloud identity: spoofable trust/forwarding
			// headers and any ambient cookies. The relay lends only the
			// node's bearer token, nothing the LAN caller injects.
			for _, h := range strippedRelayHeaders {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set("Authorization", "Bearer "+st.token)
			pr.Out.Header.Set("User-Agent", version.UserAgent())
		},
		// Flush immediately: the MCP gateway streams SSE.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			c.logger.Warn("relay request failed", "path", r.URL.Path, "error", err)
			writeError(w, http.StatusBadGateway, "control plane unreachable: "+err.Error())
		},
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.running.Load() {
			writeError(w, http.StatusServiceUnavailable, "gateway capability is not running (node mode is not \"gateway\")")
			return
		}
		if !allowedMethod(r.Method) {
			writeError(w, http.StatusMethodNotAllowed, "method not relayed")
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, RoutePrefix)
		if !ok || rest == "" || !strings.HasPrefix(rest, "/") {
			writeError(w, http.StatusNotFound, "unknown relay path")
			return
		}
		if !allowedPath(rest) {
			writeError(w, http.StatusForbidden, "path is not relayed (allowed: /api/*, /mcp)")
			return
		}
		token, err := c.tokens.Token()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable,
				"no control-plane credentials ("+noCredentialsHint+")")
			return
		}
		base, err := url.Parse(strings.TrimRight(c.getCfg().ControlPlane.URL, "/"))
		if err != nil || base.Scheme == "" || base.Host == "" {
			writeError(w, http.StatusBadGateway, "invalid control plane URL")
			return
		}
		target := *base
		target.Path = base.Path + rest
		target.RawQuery = r.URL.RawQuery
		ctx := context.WithValue(r.Context(), proxyStateKey{}, &proxyState{target: &target, token: token})
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})

	return memory.RequireNodeKey(nodeKey, inner)
}

// allowedPath restricts the relay to the control plane's API and MCP
// surfaces. Local clients authenticate to the node, and the node is the
// authenticated cloud client — OAuth and other browser-facing routes are
// deliberately not relayed.
func allowedPath(p string) bool {
	return strings.HasPrefix(p, "/api/") || p == "/mcp" || strings.HasPrefix(p, "/mcp/")
}

// allowedMethod restricts relayed methods to those the REST API and MCP
// (POST + SSE GET) surfaces actually use — no CONNECT/TRACE or other
// unexpected verbs riding the node's cloud credentials.
func allowedMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

// strippedRelayHeaders are client-supplied headers the relay never forwards
// upstream: spoofable forwarding/trust headers and ambient cookies.
var strippedRelayHeaders = []string{
	"Cookie",
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
	"X-Real-Ip",
	"Forwarded",
}

// writeError mirrors the memory API's error shape.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
