// Package adminapi is the localhost-only control surface for silo-node.
// The Silo Desktop app (or an operator's curl) drives the node through it:
// status, live config updates, auth token pushes, shutdown. It binds
// 127.0.0.1 exclusively and authenticates every request (except /healthz)
// against SILO_NODE_ADMIN_TOKEN.
package adminapi

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/onesilo/silo-node/internal/config"
)

// Controller is what the admin API needs from the node. Implemented by
// *node.Node (adminapi must not import node).
type Controller interface {
	// Status snapshots node state for GET /v1/status.
	Status(ctx context.Context) Status
	// ConfigSnapshot returns the live configuration.
	ConfigSnapshot() config.Config
	// ApplyConfigPatch merges a partial update, validates, persists to the
	// config file, triggers a reconcile, and returns the new config.
	ApplyConfigPatch(ctx context.Context, patch ConfigPatch) (config.Config, error)
	// SetJWT stores a fresh control-plane JWT (in-memory).
	SetJWT(token string)
	// SetPairingKey persists the LAN pairing key (64 hex chars).
	SetPairingKey(hexKey string) error
	// Generate runs a one-shot completion on the node's local model and
	// returns the full text plus the model tag used (POST
	// /v1/compute/generate). Errors when the compute capability is not
	// running or the Ollama call fails.
	Generate(ctx context.Context, prompt string, temperature float64) (text string, model string, err error)
	// Shutdown begins graceful node shutdown.
	Shutdown()
}

// Status is the GET /v1/status response.
type Status struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	// Mode is the node mode: "local" or "gateway".
	Mode          string             `json:"mode"`
	UptimeSeconds int64              `json:"uptime_seconds"`
	Capabilities  []CapabilityStatus `json:"capabilities"`
	Tunnel        TunnelStatus       `json:"tunnel"`
	Registration  RegistrationStatus `json:"registration"`
	LAN           LANStatus          `json:"lan"`
	Memory        MemoryStatus       `json:"memory"`
	// NodeKey authenticates memory API calls (X-Silo-Node-Key). Exposed
	// here so the desktop app / control plane can read it; the admin API
	// is localhost-only and token-authenticated.
	NodeKey string `json:"node_key,omitempty"`
}

// LANStatus is the LAN server's live state.
type LANStatus struct {
	Published bool `json:"published"` // Bonjour _silo-llm._tcp announced
	Port      int  `json:"port"`
	Clients   int  `json:"clients"` // connected WebSocket clients
}

// MemoryStatus is the memory capability's live state.
type MemoryStatus struct {
	Enabled bool `json:"enabled"`
	Healthy bool `json:"healthy"`
	Silos   int  `json:"silos"` // distinct silos with stored memories
}

// CapabilityStatus is one capability's live state.
type CapabilityStatus struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Running bool   `json:"running"`
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail"`
}

// TunnelStatus is the tunnel's live state.
type TunnelStatus struct {
	Mode string `json:"mode"`
	URL  string `json:"url"`
}

// RegistrationStatus is the control-plane registration state.
type RegistrationStatus struct {
	Registered bool   `json:"registered"`
	DeviceID   string `json:"device_id"`
	AuthMode   string `json:"auth_mode"`
	// OAuthSignedIn reports whether a usable OAuth credential is stored
	// (written by `silo-node setup` sign-in). Meaningful in gateway mode.
	OAuthSignedIn bool `json:"oauth_signed_in"`
}

// Server is the localhost admin HTTP server.
type Server struct {
	srv    *http.Server
	port   int
	logger *slog.Logger
}

// New builds the server. adminToken may be empty, in which case every
// authenticated route is refused (fail closed) — see auth.go.
func New(port int, adminToken string, ctrl Controller, logger *slog.Logger) *Server {
	mux := newMux(adminToken, ctrl, logger)
	return &Server{
		srv: &http.Server{
			Handler:           requireLoopbackHost(port, mux),
			ReadHeaderTimeout: 5 * time.Second,
		},
		port:   port,
		logger: logger,
	}
}

// requireLoopbackHost rejects requests whose Host header is not a loopback
// name. The server already binds 127.0.0.1, but a DNS-rebinding page can
// still reach it over the loopback socket with an attacker-chosen Host; an
// allowlist closes that off before any handler (even the unauthenticated
// static assets / healthz) runs. Bearer auth already stops rebinding from
// reaching data, so this is defense in depth.
func requireLoopbackHost(port int, next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, h := range []string{"127.0.0.1", "localhost", "::1", "[::1]"} {
		allowed[h] = true                             // no port
		allowed[fmt.Sprintf("%s:%d", h, port)] = true // with port
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if !allowed[host] {
			// Tolerate an explicit port that differs only cosmetically by
			// splitting host:port and re-checking the host portion.
			if h, _, err := net.SplitHostPort(host); err != nil || !allowed[h] {
				http.Error(w, "misdirected request: unexpected Host", http.StatusMisdirectedRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Start binds 127.0.0.1:<port> and serves in a background goroutine.
// Serve errors after a clean Stop are swallowed; others are reported on the
// returned channel.
func (s *Server) Start() (<-chan error, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.port))
	if err != nil {
		return nil, fmt.Errorf("binding admin API on 127.0.0.1:%d: %w", s.port, err)
	}
	s.logger.Info("admin API listening", "addr", ln.Addr().String())
	errCh := make(chan error, 1)
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()
	return errCh, nil
}

// Stop shuts the server down gracefully.
func (s *Server) Stop(ctx context.Context) {
	if err := s.srv.Shutdown(ctx); err != nil {
		s.logger.Warn("admin API shutdown", "error", err)
	}
}
