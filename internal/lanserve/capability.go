package lanserve

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/onesilo/onesilo-node/internal/config"
)

// bonjourRefreshInterval is how often the announcer re-checks the current
// model (re-announcing when it changes) and the lan.enabled toggle.
const bonjourRefreshInterval = 15 * time.Second

// Capability runs the LAN server as a node capability. It is enabled when
// LAN serving is on OR the memory capability is on OR the node is a
// control-plane gateway: the memory HTTP API and the gateway relay ride
// the same server. Bonjour is only published while lan.enabled is true
// (memory-only and relay-only nodes don't advertise an LLM service).
type Capability struct {
	getCfg       func() config.Config
	compute      ComputeBackend
	apiHandler   http.Handler
	currentModel func() string
	keySource    func() []byte
	newAnnouncer func() Announcer
	onClients    func(int)
	pairer       *Pairer
	logger       *slog.Logger

	mu        sync.Mutex
	server    *Server
	announcer Announcer
	published bool
	lastModel string
	cancel    context.CancelFunc
}

// NewCapability wires the LAN capability.
//   - compute may be nil (memory-only node).
//   - apiHandler serves the node HTTP APIs under /v1/ (memory, gateway
//     relay) and may be nil.
//   - currentModel feeds the Bonjour TXT record.
//   - keySource resolves the pairing key per message (see FileKeySource).
//   - newAnnouncer is a factory so tests can inject a fake.
func NewCapability(
	getCfg func() config.Config,
	compute ComputeBackend,
	apiHandler http.Handler,
	currentModel func() string,
	keySource func() []byte,
	newAnnouncer func() Announcer,
	logger *slog.Logger,
) *Capability {
	if newAnnouncer == nil {
		newAnnouncer = NewZeroconfAnnouncer
	}
	return &Capability{
		getCfg:       getCfg,
		compute:      compute,
		apiHandler:   apiHandler,
		currentModel: currentModel,
		keySource:    keySource,
		newAnnouncer: newAnnouncer,
		logger:       logger,
	}
}

// Name implements node.Capability.
func (c *Capability) Name() string { return "lan" }

// Enabled implements node.Capability: the LAN server must run for LAN
// serving, for the memory API, for the gateway relay (same port), for the
// OpenAI-compatible surface, or when the node is exposed — the tunnel
// targets this server, so it must be up for remote apps to reach the
// LLM/memory endpoints.
func (c *Capability) Enabled() bool {
	cfg := c.getCfg()
	return cfg.LAN.Enabled || cfg.Capabilities.Memory ||
		cfg.Mode == config.ModeGateway || cfg.Exposed() ||
		(cfg.OpenAI.Enabled && cfg.Capabilities.Compute)
}

// Start implements node.Capability: bind the server, publish Bonjour when
// LAN serving is enabled, and keep the TXT record's model fresh.
func (c *Capability) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.server != nil {
		return nil
	}

	router := NewRouter(c.keySource, c.compute, c.logger.With("component", "router"))
	router.SetPairer(c.pairer)
	server := NewServer(c.getCfg().LAN.Port, router, c.apiHandler, c.onClients, c.logger)
	if err := server.Start(ctx); err != nil {
		return err
	}
	c.server = server

	watchCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.reconcileBonjourLocked()
	go c.watchBonjour(watchCtx)
	return nil
}

// Stop implements node.Capability.
func (c *Capability) Stop(ctx context.Context) error {
	c.mu.Lock()
	server := c.server
	cancel := c.cancel
	c.server = nil
	c.cancel = nil
	c.unpublishLocked()
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if server != nil {
		server.Stop(ctx)
	}
	c.logger.Info("LAN capability stopped")
	return nil
}

// Healthy implements node.Capability.
func (c *Capability) Healthy(ctx context.Context) (bool, string) {
	c.mu.Lock()
	server := c.server
	published := c.published
	c.mu.Unlock()
	if server == nil || !server.Running() {
		return false, "not started"
	}
	detail := fmt.Sprintf("serving on %s (%d clients)", server.Addr(), server.ClientCount())
	if published {
		detail += ", bonjour published"
	}
	return true, detail
}

// SetOnClients registers a client-count callback (admin status refresh).
// Must be called before Start.
func (c *Capability) SetOnClients(fn func(int)) { c.onClients = fn }

// SetPairer enables automated device pairing on the LAN router. Must be
// called before Start; a nil pairer leaves the router on the file-key path.
func (c *Capability) SetPairer(p *Pairer) { c.pairer = p }

// Pairer returns the configured pairer (nil when automated pairing is off),
// so the admin API can list/verify pending pairings.
func (c *Capability) Pairer() *Pairer { return c.pairer }

// Published reports whether the Bonjour service is currently announced.
func (c *Capability) Published() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.published
}

// Clients returns the connected WebSocket client count (0 when stopped).
func (c *Capability) Clients() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.server == nil {
		return 0
	}
	return c.server.ClientCount()
}

// Port returns the configured LAN port.
func (c *Capability) Port() int { return c.getCfg().LAN.Port }

// watchBonjour keeps the announcement in sync with config (lan.enabled can
// flip while the server stays up for memory) and re-announces when the
// served model changes.
func (c *Capability) watchBonjour(ctx context.Context) {
	ticker := time.NewTicker(bonjourRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			if c.server == nil {
				c.mu.Unlock()
				return
			}
			c.reconcileBonjourLocked()
			c.mu.Unlock()
		}
	}
}

func (c *Capability) reconcileBonjourLocked() {
	want := c.getCfg().LAN.Enabled
	model := ""
	if c.currentModel != nil {
		model = c.currentModel()
	}

	switch {
	case want && !c.published:
		ann := c.newAnnouncer()
		if err := ann.Announce(BonjourInstanceName(), c.getCfg().LAN.Port, BonjourTXT(model)); err != nil {
			c.logger.Warn("bonjour announce failed, will retry", "error", err)
			return
		}
		c.announcer = ann
		c.published = true
		c.lastModel = model
		c.logger.Info("bonjour service published", "instance", BonjourInstanceName(), "model", model)
	case !want && c.published:
		c.unpublishLocked()
	case want && c.published && model != c.lastModel:
		if err := c.announcer.Update(BonjourTXT(model)); err != nil {
			c.logger.Warn("bonjour TXT update failed", "error", err)
			return
		}
		c.lastModel = model
		c.logger.Info("bonjour TXT updated", "model", model)
	}
}

func (c *Capability) unpublishLocked() {
	if c.announcer != nil {
		c.announcer.Shutdown()
		c.announcer = nil
	}
	if c.published {
		c.logger.Info("bonjour service unpublished")
	}
	c.published = false
	c.lastModel = ""
}
