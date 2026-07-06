package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// CapabilityProbe is the slice of the node capability interface the
// registration manager needs (satisfied structurally by node capabilities).
type CapabilityProbe interface {
	Name() string
	Enabled() bool
	Healthy(ctx context.Context) (bool, string)
}

// capabilityStatusKeys maps node capability names to the control plane's
// capability identifiers. The memory capability lands in a later phase; its
// mapping is already wired so enabling it starts reporting automatically.
var capabilityStatusKeys = map[string][]string{
	"compute": {"llm_inference"},
	"memory":  {"silo_recall", "silo_remember"},
}

// Liveness values sent in capabilities_status.
const (
	statusLive = "live"
	statusDead = "dead"
)

const (
	defaultHeartbeatInterval = 30 * time.Second
	registerRetryInterval    = time.Minute
	// idleInterval parks the loop while registration isn't desired or auth
	// is missing; kicks (config/tunnel/token changes) wake it early.
	idleInterval = time.Hour
	probeTimeout = 5 * time.Second
)

// Manager owns the register / heartbeat / delete lifecycle against the
// control plane. Single goroutine (Run); external events arrive as kicks.
type Manager struct {
	client       *Client
	logger       *slog.Logger
	dataDir      string
	deviceName   func() string
	modelName    func() string
	capabilities func() []CapabilityProbe

	kick chan struct{}

	mu            sync.Mutex
	deviceID      string
	tunnelURL     string
	registered    bool
	registeredURL string
	authBlocked   bool
	interval      time.Duration
}

// NewManager builds the registration manager. deviceName/modelName/
// capabilities are getters over live node state.
func NewManager(
	client *Client,
	dataDir string,
	deviceName func() string,
	modelName func() string,
	capabilities func() []CapabilityProbe,
	logger *slog.Logger,
) *Manager {
	return &Manager{
		client:       client,
		logger:       logger,
		dataDir:      dataDir,
		deviceName:   deviceName,
		modelName:    modelName,
		capabilities: capabilities,
		kick:         make(chan struct{}, 1),
		interval:     defaultHeartbeatInterval,
	}
}

// SetTunnelURL records the current public URL ("" when the tunnel is down)
// and wakes the loop; a changed URL triggers re-registration.
func (m *Manager) SetTunnelURL(url string) {
	m.mu.Lock()
	changed := m.tunnelURL != url
	m.tunnelURL = url
	m.mu.Unlock()
	if changed {
		m.Kick()
	}
}

// NotifyTokenUpdated clears the auth backoff after the desktop app pushes a
// fresh JWT (or the API key changes) and wakes the loop.
func (m *Manager) NotifyTokenUpdated() {
	m.mu.Lock()
	m.authBlocked = false
	m.mu.Unlock()
	m.Kick()
}

// Kick wakes the loop early (config changes, capability toggles).
func (m *Manager) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Status returns the current registration state for the admin API.
func (m *Manager) Status() (registered bool, deviceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.registered, m.deviceID
}

// Run drives the lifecycle until ctx is cancelled, then deletes the
// registration (graceful shutdown).
func (m *Manager) Run(ctx context.Context) {
	for {
		wait := m.step(ctx)
		select {
		case <-ctx.Done():
			m.deregister()
			return
		case <-m.kick:
		case <-time.After(wait):
		}
	}
}

// step performs one action (register / heartbeat / deregister / nothing)
// and returns how long to wait before the next one.
func (m *Manager) step(ctx context.Context) time.Duration {
	m.mu.Lock()
	url := m.tunnelURL
	registered := m.registered
	registeredURL := m.registeredURL
	blocked := m.authBlocked
	interval := m.interval
	m.mu.Unlock()

	capabilities := m.enabledCapabilityIDs()
	// Registration only happens when at least one capability is enabled AND
	// a public URL is available (quick-tunnel URL or configured external
	// URL — the node feeds whichever applies through SetTunnelURL).
	desired := url != "" && len(capabilities) > 0

	if !desired {
		if registered {
			m.deregister()
		}
		return idleInterval
	}
	if blocked {
		return idleInterval // woken by NotifyTokenUpdated
	}
	if !registered || registeredURL != url {
		return m.register(ctx, url, capabilities)
	}
	return m.heartbeat(ctx, interval)
}

func (m *Manager) register(ctx context.Context, url string, capabilities []string) time.Duration {
	deviceID, err := m.ensureDeviceID()
	if err != nil {
		m.logger.Error("cannot persist device id", "error", err)
		return registerRetryInterval
	}

	resp, err := m.client.Register(ctx, RegisterRequest{
		TunnelURL:          url,
		DeviceName:         m.deviceName(),
		ModelName:          m.modelName(),
		Capabilities:       capabilities,
		DeviceID:           deviceID,
		CapabilitiesStatus: m.capabilitiesStatus(ctx),
	})
	switch {
	case err == nil:
	case errors.Is(err, ErrNoToken) || IsStatus(err, http.StatusUnauthorized):
		m.logger.Info("registration waiting for auth token", "error", err)
		m.setAuthBlocked()
		return idleInterval
	case IsStatus(err, http.StatusNotFound):
		// Backend endpoints not deployed yet — expected during rollout.
		m.logger.Warn("destinations API not available yet (404), will retry", "url", m.client.baseURL())
		return registerRetryInterval
	default:
		m.logger.Warn("registration failed, will retry", "error", err)
		return registerRetryInterval
	}

	interval := defaultHeartbeatInterval
	if resp.HeartbeatIntervalSeconds > 0 {
		interval = time.Duration(resp.HeartbeatIntervalSeconds) * time.Second
	}
	if resp.DeviceID != "" && resp.DeviceID != deviceID {
		// Server minted / normalized the id: adopt and persist it.
		if err := SaveDeviceID(m.dataDir, resp.DeviceID); err != nil {
			m.logger.Warn("could not persist server-issued device id", "error", err)
		}
		deviceID = resp.DeviceID
	}

	m.mu.Lock()
	m.deviceID = deviceID
	m.registered = true
	m.registeredURL = url
	m.interval = interval
	m.mu.Unlock()
	m.logger.Info("registered with control plane",
		"device_id", deviceID, "tunnel_url", url,
		"capabilities", capabilities, "heartbeat_interval", interval)
	return interval
}

func (m *Manager) heartbeat(ctx context.Context, interval time.Duration) time.Duration {
	m.mu.Lock()
	deviceID := m.deviceID
	m.mu.Unlock()

	err := m.client.Heartbeat(ctx, deviceID, m.capabilitiesStatus(ctx))
	switch {
	case err == nil:
		m.logger.Debug("heartbeat sent", "device_id", deviceID)
		return interval
	case errors.Is(err, ErrNoToken) || IsStatus(err, http.StatusUnauthorized):
		m.logger.Info("heartbeat unauthorized, waiting for fresh token")
		m.setAuthBlocked()
		m.setUnregistered()
		return idleInterval
	case IsStatus(err, http.StatusNotFound):
		// Registration expired or endpoint was rolled back — re-register.
		m.logger.Warn("heartbeat 404, re-registering", "device_id", deviceID)
		m.setUnregistered()
		return time.Second
	default:
		m.logger.Warn("heartbeat failed", "error", err)
		return interval
	}
}

// deregister deletes the registration; used on shutdown and when the last
// capability or the tunnel goes away. Uses its own timeout because the run
// context is typically already cancelled.
func (m *Manager) deregister() {
	m.mu.Lock()
	registered := m.registered
	deviceID := m.deviceID
	m.registered = false
	m.registeredURL = ""
	m.mu.Unlock()
	if !registered {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.client.Delete(ctx, deviceID); err != nil {
		// Best-effort cleanup; the TTL reaps us server-side regardless.
		m.logger.Warn("deregister failed (ignored)", "error", err)
		return
	}
	m.logger.Info("deregistered from control plane", "device_id", deviceID)
}

func (m *Manager) setAuthBlocked() {
	m.mu.Lock()
	m.authBlocked = true
	m.mu.Unlock()
}

func (m *Manager) setUnregistered() {
	m.mu.Lock()
	m.registered = false
	m.registeredURL = ""
	m.mu.Unlock()
}

func (m *Manager) ensureDeviceID() (string, error) {
	m.mu.Lock()
	id := m.deviceID
	m.mu.Unlock()
	if id != "" {
		return id, nil
	}
	id, err := LoadOrCreateDeviceID(m.dataDir)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.deviceID = id
	m.mu.Unlock()
	return id, nil
}

// enabledCapabilityIDs maps enabled node capabilities to control-plane
// capability identifiers (compute -> llm_inference; memory -> silo_recall +
// silo_remember).
func (m *Manager) enabledCapabilityIDs() []string {
	var ids []string
	for _, p := range m.capabilities() {
		if !p.Enabled() {
			continue
		}
		ids = append(ids, capabilityStatusKeys[p.Name()]...)
	}
	return ids
}

// capabilitiesStatus probes each enabled capability and maps its health to
// the wire form. Disabled capabilities are omitted entirely.
func (m *Manager) capabilitiesStatus(ctx context.Context) map[string]string {
	status := map[string]string{}
	for _, p := range m.capabilities() {
		if !p.Enabled() {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		healthy, _ := p.Healthy(probeCtx)
		cancel()
		value := statusDead
		if healthy {
			value = statusLive
		}
		for _, key := range capabilityStatusKeys[p.Name()] {
			status[key] = value
		}
	}
	if len(status) == 0 {
		return nil
	}
	return status
}
