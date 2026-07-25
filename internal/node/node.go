// Package node owns the silo-node lifecycle: a single reconciler that
// starts/stops capability goroutines, the quick tunnel, and control-plane
// registration to match the live configuration. The admin API drives it.
package node

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/onesilo/silo-node/internal/adminapi"
	"github.com/onesilo/silo-node/internal/compute"
	"github.com/onesilo/silo-node/internal/config"
	"github.com/onesilo/silo-node/internal/controlplane"
	"github.com/onesilo/silo-node/internal/lanserve"
	"github.com/onesilo/silo-node/internal/memory"
	"github.com/onesilo/silo-node/internal/tunnel"
	"github.com/onesilo/silo-node/internal/version"
)

// reconcileInterval doubles as the retry cadence for capabilities that
// failed to start (e.g. Ollama not installed yet).
const reconcileInterval = 30 * time.Second

var pairingKeyPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// Node is the running silo-node instance.
type Node struct {
	logger     *slog.Logger
	configPath string
	adminToken string

	// cfgMu guards cfg only; reconcileMu serializes Reconcile.
	cfgMu sync.RWMutex
	cfg   config.Config

	reconcileMu sync.Mutex

	computeCap   *compute.Capability
	memoryCap    *memory.Capability
	lanCap       *lanserve.Capability
	nodeKey      string
	capabilities []Capability
	capRunning   map[string]bool

	jwtStore *controlplane.JWTStore
	regMgr   *controlplane.Manager

	tunnelMu  sync.Mutex
	tunnelMgr *tunnel.Manager

	runCtx  context.Context
	cancel  context.CancelFunc
	started time.Time
}

// New builds a node from the loaded config. configPath is where admin-API
// config updates are persisted. adminToken guards the admin API (from
// SILO_NODE_ADMIN_TOKEN; empty fails closed).
func New(cfg config.Config, configPath, adminToken string, logger *slog.Logger) (*Node, error) {
	n := &Node{
		logger:     logger,
		configPath: configPath,
		adminToken: adminToken,
		cfg:        cfg,
		capRunning: map[string]bool{},
		jwtStore:   &controlplane.JWTStore{},
	}

	dataDir, err := cfg.ResolvedDataDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir %s: %w", dataDir, err)
	}

	// Node key: authenticates the memory API (X-Silo-Node-Key); created on
	// first start, surfaced via admin GET /v1/status.
	n.nodeKey, err = memory.LoadOrCreateNodeKey(dataDir)
	if err != nil {
		return nil, err
	}

	n.computeCap = compute.New(n.snapshot, logger.With("capability", "compute"))

	// Memory embeds via the compute capability's Ollama client when compute
	// is enabled; otherwise recall degrades to keyword-only.
	n.memoryCap = memory.New(n.snapshot, func() (memory.Embedder, string, bool) {
		cfg := n.snapshot()
		if !cfg.Capabilities.Compute {
			return nil, "", false
		}
		return n.computeCap, cfg.Memory.EmbedModel, true
	}, logger.With("capability", "memory"))

	// The LAN server runs when lan.enabled OR memory is on: the memory API
	// is mounted on the same port. Bonjour only advertises while
	// lan.enabled (see internal/lanserve).
	n.lanCap = lanserve.NewCapability(
		n.snapshot,
		n.computeCap,
		n.memoryCap.Handler(func() string { return n.nodeKey }),
		func() string { return n.computeCap.CurrentModel() },
		lanserve.FileKeySource(func() (string, error) {
			cfg := n.snapshot()
			return cfg.ResolvedDataDir()
		}),
		nil, // production zeroconf announcer
		logger.With("capability", "lan"),
	)

	// The heartbeat maps compute -> llm_inference and memory ->
	// silo_recall/silo_remember; "lan" has no control-plane identifier and
	// is reported only through the admin API.
	n.capabilities = []Capability{n.computeCap, n.memoryCap, n.lanCap}

	tokens := &controlplane.ModalTokenSource{
		Mode:   func() string { return n.snapshot().ControlPlane.AuthMode },
		JWT:    n.jwtStore,
		APIKey: controlplane.NewAPIKeyStore(),
	}
	client := controlplane.NewClient(func() string { return n.snapshot().ControlPlane.URL }, tokens)
	n.regMgr = controlplane.NewManager(
		client,
		dataDir,
		n.deviceName,
		func() string { return n.computeCap.CurrentModel() },
		n.capabilityProbes,
		logger.With("component", "controlplane"),
	)
	return n, nil
}

func (n *Node) snapshot() config.Config {
	n.cfgMu.RLock()
	defer n.cfgMu.RUnlock()
	return n.cfg
}

func (n *Node) deviceName() string {
	if name := n.snapshot().ControlPlane.DeviceName; name != "" {
		return name
	}
	if host, err := os.Hostname(); err == nil {
		return host
	}
	return "silo-node"
}

func (n *Node) capabilityProbes() []controlplane.CapabilityProbe {
	probes := make([]controlplane.CapabilityProbe, len(n.capabilities))
	for i, c := range n.capabilities {
		probes[i] = c
	}
	return probes
}

// Run drives the node until ctx is cancelled (signal or admin shutdown),
// then tears everything down gracefully: deregister from the control
// plane, stop capabilities and the tunnel, stop the admin API.
func (n *Node) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	n.runCtx = runCtx
	n.cancel = cancel
	n.started = time.Now()
	defer cancel()

	admin := adminapi.New(n.snapshot().Admin.Port, n.adminToken, n, n.logger.With("component", "adminapi"))
	adminErr, err := admin.Start()
	if err != nil {
		return err
	}

	regDone := make(chan struct{})
	go func() {
		defer close(regDone)
		n.regMgr.Run(runCtx)
	}()

	n.logger.Info("silo-node started",
		"version", version.Version, "commit", version.Commit,
		"admin_port", n.snapshot().Admin.Port)
	n.Reconcile(runCtx)

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-runCtx.Done():
			break loop
		case err, ok := <-adminErr:
			if ok && err != nil {
				n.logger.Error("admin API failed", "error", err)
				cancel()
			}
			adminErr = nil // drained; don't spin on the closed channel
		case <-ticker.C:
			n.Reconcile(runCtx)
		}
	}

	n.logger.Info("shutting down")
	// 1. Registration manager deletes our destination (it uses its own
	//    short-timeout context since runCtx is already cancelled).
	<-regDone
	// 2. Stop capabilities and the tunnel.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	for _, c := range n.capabilities {
		if n.capRunning[c.Name()] {
			if err := c.Stop(stopCtx); err != nil {
				n.logger.Warn("capability stop failed", "capability", c.Name(), "error", err)
			}
			n.capRunning[c.Name()] = false
		}
	}
	n.stopTunnel()
	// 3. Stop the admin API last so status stays queryable during teardown.
	admin.Stop(stopCtx)
	n.logger.Info("shutdown complete")
	return nil
}

// Reconcile applies the live config: starts/stops capabilities and the
// tunnel, and updates the registration manager's desired state. Safe to
// call from the admin API and the periodic ticker concurrently.
func (n *Node) Reconcile(ctx context.Context) {
	n.reconcileMu.Lock()
	defer n.reconcileMu.Unlock()

	cfg := n.snapshot()

	for _, c := range n.capabilities {
		name := c.Name()
		switch {
		case c.Enabled() && !n.capRunning[name]:
			n.logger.Info("starting capability", "capability", name)
			if err := c.Start(ctx); err != nil {
				n.logger.Warn("capability start failed, will retry", "capability", name, "error", err)
				continue
			}
			n.capRunning[name] = true
		case !c.Enabled() && n.capRunning[name]:
			n.logger.Info("stopping capability", "capability", name)
			if err := c.Stop(ctx); err != nil {
				n.logger.Warn("capability stop failed", "capability", name, "error", err)
			}
			n.capRunning[name] = false
		}
	}

	anyEnabled := false
	for _, c := range n.capabilities {
		if c.Enabled() {
			anyEnabled = true
			break
		}
	}

	// Quick tunnel: run only while a capability needs to be reachable.
	wantQuick := cfg.Tunnel.Mode == config.TunnelModeQuick && anyEnabled
	n.tunnelMu.Lock()
	running := n.tunnelMgr != nil
	if wantQuick && !running {
		mgr := tunnel.New(cfg.LAN.Port, cfg.Tunnel.CloudflaredPath,
			n.logger.With("component", "tunnel"), n.onTunnelURLChange)
		if err := mgr.Start(n.runCtx); err != nil {
			n.logger.Warn("tunnel start failed, will retry", "error", err)
		} else {
			n.tunnelMgr = mgr
		}
	} else if !wantQuick && running {
		mgr := n.tunnelMgr
		n.tunnelMgr = nil
		n.tunnelMu.Unlock()
		mgr.Stop() // fires onTunnelURLChange("")
		n.tunnelMu.Lock()
	}
	n.tunnelMu.Unlock()

	// Registration desired state: quick-tunnel URL, external URL, or none.
	switch cfg.Tunnel.Mode {
	case config.TunnelModeQuick:
		n.regMgr.SetTunnelURL(n.tunnelURL())
	case config.TunnelModeExternal:
		n.regMgr.SetTunnelURL(cfg.Tunnel.ExternalURL)
	default:
		n.regMgr.SetTunnelURL("")
	}
	n.regMgr.Kick()
}

func (n *Node) onTunnelURLChange(url string) {
	if url == "" {
		n.logger.Info("tunnel down")
	} else {
		n.logger.Info("tunnel URL changed", "url", url)
	}
	// Only quick mode feeds the registration manager from the tunnel.
	if n.snapshot().Tunnel.Mode == config.TunnelModeQuick {
		n.regMgr.SetTunnelURL(url)
	}
}

func (n *Node) tunnelURL() string {
	n.tunnelMu.Lock()
	defer n.tunnelMu.Unlock()
	if n.tunnelMgr == nil {
		return ""
	}
	return n.tunnelMgr.URL()
}

func (n *Node) stopTunnel() {
	n.tunnelMu.Lock()
	mgr := n.tunnelMgr
	n.tunnelMgr = nil
	n.tunnelMu.Unlock()
	if mgr != nil {
		mgr.Stop()
	}
}

// --- adminapi.Controller implementation ---

// Status implements adminapi.Controller.
func (n *Node) Status(ctx context.Context) adminapi.Status {
	cfg := n.snapshot()
	caps := make([]adminapi.CapabilityStatus, 0, len(n.capabilities))
	memoryHealthy := false
	for _, c := range n.capabilities {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		healthy, detail := c.Healthy(probeCtx)
		cancel()
		if c.Name() == "memory" {
			memoryHealthy = healthy
		}
		n.reconcileMu.Lock()
		running := n.capRunning[c.Name()]
		n.reconcileMu.Unlock()
		caps = append(caps, adminapi.CapabilityStatus{
			Name:    c.Name(),
			Enabled: c.Enabled(),
			Running: running,
			Healthy: healthy,
			Detail:  detail,
		})
	}
	siloCount := 0
	if silos, err := n.memoryCap.Silos(ctx); err == nil {
		siloCount = len(silos)
	}
	registered, deviceID := n.regMgr.Status()
	tunnelURL := n.tunnelURL()
	if cfg.Tunnel.Mode == config.TunnelModeExternal {
		tunnelURL = cfg.Tunnel.ExternalURL
	}
	return adminapi.Status{
		Version:       version.Version,
		Commit:        version.Commit,
		UptimeSeconds: int64(time.Since(n.started).Seconds()),
		Capabilities:  caps,
		Tunnel:        adminapi.TunnelStatus{Mode: cfg.Tunnel.Mode, URL: tunnelURL},
		Registration: adminapi.RegistrationStatus{
			Registered: registered,
			DeviceID:   deviceID,
			AuthMode:   cfg.ControlPlane.AuthMode,
		},
		LAN: adminapi.LANStatus{
			Published: n.lanCap.Published(),
			Port:      cfg.LAN.Port,
			Clients:   n.lanCap.Clients(),
		},
		Memory: adminapi.MemoryStatus{
			Enabled: cfg.Capabilities.Memory,
			Healthy: memoryHealthy,
			Silos:   siloCount,
		},
		NodeKey: n.nodeKey,
	}
}

// ConfigSnapshot implements adminapi.Controller.
func (n *Node) ConfigSnapshot() config.Config { return n.snapshot() }

// ApplyConfigPatch implements adminapi.Controller: merge, validate,
// persist to the config file, reconcile.
func (n *Node) ApplyConfigPatch(ctx context.Context, patch adminapi.ConfigPatch) (config.Config, error) {
	n.cfgMu.Lock()
	next := n.cfg
	patch.ApplyTo(&next)
	if err := next.Validate(); err != nil {
		n.cfgMu.Unlock()
		return n.cfg, err
	}
	if err := config.Save(next, n.configPath); err != nil {
		n.cfgMu.Unlock()
		return n.cfg, fmt.Errorf("persisting config: %w", err)
	}
	n.cfg = next
	n.cfgMu.Unlock()

	n.Reconcile(n.runCtx)
	return next, nil
}

// SetJWT implements adminapi.Controller: store the pushed token and wake
// the registration manager (clears any 401 backoff).
func (n *Node) SetJWT(token string) {
	n.jwtStore.Set(token)
	n.regMgr.NotifyTokenUpdated()
}

// SetPairingKey implements adminapi.Controller: persist the legacy LAN
// pairing key to <data_dir>/pairing.key with 0600.
func (n *Node) SetPairingKey(hexKey string) error {
	if !pairingKeyPattern.MatchString(hexKey) {
		return fmt.Errorf("pairing key must be exactly 64 hex characters")
	}
	cfg := n.snapshot()
	dataDir, err := cfg.ResolvedDataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	path := filepath.Join(dataDir, "pairing.key")
	if err := os.WriteFile(path, []byte(hexKey), 0o600); err != nil {
		return fmt.Errorf("writing pairing key: %w", err)
	}
	return nil
}

// Generate implements adminapi.Controller: collect a one-shot completion
// from the compute capability (POST /v1/compute/generate). This is how
// local clients — e.g. a Buzz agent distilling conversation privately
// before anything reaches the control plane — borrow the node's model.
func (n *Node) Generate(ctx context.Context, prompt string, temperature float64) (string, string, error) {
	stream, err := n.computeCap.StreamGenerate(ctx, prompt, temperature)
	if err != nil {
		return "", "", err
	}
	var b strings.Builder
	for res := range stream {
		if res.Err != nil {
			return "", "", res.Err
		}
		b.WriteString(res.Delta.Response)
	}
	return b.String(), n.computeCap.CurrentModel(), nil
}

// Shutdown implements adminapi.Controller.
func (n *Node) Shutdown() {
	if n.cancel != nil {
		n.cancel()
	}
}
