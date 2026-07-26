package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Managed named tunnels: the control plane provisions a per-node Cloudflare
// named tunnel (stable <slug>.tunnel.onesilo.com hostname) and hands us a
// run token; we spawn `cloudflared tunnel run` with the token in the
// TUNNEL_TOKEN environment variable (never argv — argv is visible to every
// local process via ps). Unlike quick tunnels there is no URL to scrape:
// the public URL is known before cloudflared starts, so readiness is
// detected from cloudflared's "Registered tunnel connection" log line.

// ErrSubscriptionRequired marks a provisioning refusal because the owner's
// plan doesn't include remote access (control-plane 402). The provision
// callback maps its transport-specific error to this sentinel (wrapped is
// fine) so the manager can back off long and surface the state.
var ErrSubscriptionRequired = errors.New("remote access requires a paid One Silo subscription")

// ErrManagedUnavailable marks a control plane that cannot provision managed
// tunnels (404 on an old deploy, 503 when Cloudflare isn't configured).
var ErrManagedUnavailable = errors.New("control plane cannot provision managed tunnels")

// readyPattern matches cloudflared's named-tunnel readiness log line.
var readyPattern = regexp.MustCompile(`Registered tunnel connection`)

// subscriptionBackoff paces provisioning retries while the owner is on the
// free tier, so an upgrade is picked up without a restart but the control
// plane isn't hammered. Mirrors controlplane's subscriptionRetryInterval.
const subscriptionBackoff = 10 * time.Minute

// unavailableBackoff paces retries when the control plane can't provision
// managed tunnels at all (bad deploy / missing Cloudflare config).
const unavailableBackoff = 5 * time.Minute

// Provisioned is what the provision callback returns: the tunnel's public
// URL (stable across restarts) and the run token for this start.
type Provisioned struct {
	URL   string
	Token string
}

// ManagedManager keeps one control-plane-provisioned cloudflared named
// tunnel alive. Same lifecycle shape as the quick-tunnel Manager: Start is
// idempotent, Stop terminates the process and waits for the loop.
type ManagedManager struct {
	// provision asks the control plane for the tunnel URL + a fresh run
	// token. Called before every cloudflared (re)start — tokens are never
	// persisted, so each start re-fetches.
	provision  func(ctx context.Context) (Provisioned, error)
	binaryPath string // optional configured cloudflared override
	logger     *slog.Logger
	// onURLChange is called with the public URL when the tunnel comes up and
	// with "" when it goes down. Called from the manager goroutine.
	onURLChange func(string)

	mu                  sync.Mutex
	url                 string
	running             bool
	subscriptionBlocked bool
	cancel              context.CancelFunc
	done                chan struct{}
}

// NewManaged builds a manager. onURLChange may be nil.
func NewManaged(
	provision func(ctx context.Context) (Provisioned, error),
	binaryPath string,
	logger *slog.Logger,
	onURLChange func(string),
) *ManagedManager {
	return &ManagedManager{
		provision:   provision,
		binaryPath:  binaryPath,
		logger:      logger,
		onURLChange: onURLChange,
	}
}

// URL returns the current public tunnel URL ("" while down).
func (m *ManagedManager) URL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.url
}

// SubscriptionBlocked reports whether the last provisioning attempt was
// refused because remote access needs a paid plan.
func (m *ManagedManager) SubscriptionBlocked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subscriptionBlocked
}

// Start launches the tunnel loop. Idempotent. Fails fast when cloudflared
// is not installed; provisioning failures are retried inside the loop.
func (m *ManagedManager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	if _, err := FindBinary(m.binaryPath); err != nil {
		m.mu.Unlock()
		return err
	}
	loopCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})
	m.running = true
	done := m.done
	m.mu.Unlock()

	go func() {
		defer close(done)
		m.runLoop(loopCtx)
	}()
	return nil
}

// Stop terminates the tunnel process and waits for the loop to exit.
func (m *ManagedManager) Stop() {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	m.running = false
	cancel := m.cancel
	done := m.done
	m.mu.Unlock()

	cancel()
	<-done
}

// backoffFor classifies a provisioning error into the retry delay to use.
// Exposed as a function (not a method) for direct unit testing.
func backoffFor(err error, transient time.Duration) time.Duration {
	switch {
	case errors.Is(err, ErrSubscriptionRequired):
		return subscriptionBackoff
	case errors.Is(err, ErrManagedUnavailable):
		return unavailableBackoff
	default:
		return transient
	}
}

// runLoop provisions and keeps one cloudflared process alive, re-provisioning
// (fresh token) before every restart, with exponential backoff on transient
// failures and long fixed backoff on subscription/availability refusals.
func (m *ManagedManager) runLoop(ctx context.Context) {
	defer m.setURL("")
	backoff := initialBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		prov, err := m.provision(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.setSubscriptionBlocked(errors.Is(err, ErrSubscriptionRequired))
			wait := backoffFor(err, backoff)
			switch {
			case errors.Is(err, ErrSubscriptionRequired):
				m.logger.Warn("remote access requires a paid One Silo subscription — " +
					"the node keeps working on your local network; upgrade at onesilo.com to be reachable from anywhere")
			case errors.Is(err, ErrManagedUnavailable):
				m.logger.Warn("control plane cannot provision managed tunnels; "+
					"retrying — or set tunnel.mode = \"quick\" to use an ephemeral tunnel", "error", err)
			default:
				m.logger.Warn("tunnel provisioning failed, retrying", "error", err, "backoff", wait)
				backoff = min(backoff*2, maxBackoff)
			}
			if !sleepCtx(ctx, wait) {
				return
			}
			continue
		}
		m.setSubscriptionBlocked(false)

		cmd, err := m.spawn(ctx, prov.Token)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.logger.Warn("cloudflared start failed, retrying", "error", err, "backoff", backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = initialBackoff
		m.logger.Info("managed tunnel ready", "url", prov.URL)
		m.setURL(prov.URL)

		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		select {
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
				<-exited
			}
			return
		case err := <-exited:
			m.setURL("")
			m.logger.Warn("cloudflared exited unexpectedly, restarting", "error", err, "backoff", backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

// spawn starts `cloudflared tunnel run` with the token in TUNNEL_TOKEN and
// waits (up to readyTimeout) for the connection-registered log line. On
// error the process is reaped.
func (m *ManagedManager) spawn(ctx context.Context, token string) (*exec.Cmd, error) {
	binary, err := FindBinary(m.binaryPath)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(binary, "tunnel", "run")
	cmd.Env = append(os.Environ(), "TUNNEL_TOKEN="+token)
	readyCh := make(chan struct{}, 2)
	cmd.Stdout = &readyScanWriter{ready: readyCh, logger: m.logger}
	cmd.Stderr = &readyScanWriter{ready: readyCh, logger: m.logger}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting cloudflared: %w", err)
	}
	m.logger.Info("cloudflared started", "binary", binary, "pid", cmd.Process.Pid)

	select {
	case <-readyCh:
		return cmd, nil
	case <-time.After(readyTimeout):
		m.reapCmd(cmd)
		return nil, ErrURLTimeout
	case <-ctx.Done():
		m.reapCmd(cmd)
		return nil, ctx.Err()
	}
}

func (m *ManagedManager) reapCmd(cmd *exec.Cmd) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
}

func (m *ManagedManager) setURL(url string) {
	m.mu.Lock()
	changed := m.url != url
	m.url = url
	cb := m.onURLChange
	m.mu.Unlock()
	if changed && cb != nil {
		cb(url)
	}
}

func (m *ManagedManager) setSubscriptionBlocked(blocked bool) {
	m.mu.Lock()
	m.subscriptionBlocked = blocked
	m.mu.Unlock()
}

// readyScanWriter scans cloudflared output line-by-line for the named-tunnel
// readiness marker and signals once. Mirrors urlScanWriter; output is
// discarded after the marker is seen.
type readyScanWriter struct {
	ready  chan<- struct{}
	logger *slog.Logger

	buf   strings.Builder
	found bool
}

func (w *readyScanWriter) Write(p []byte) (int, error) {
	if w.found {
		return len(p), nil
	}
	w.buf.Write(p)
	for {
		s := w.buf.String()
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		line := s[:i]
		w.buf.Reset()
		w.buf.WriteString(s[i+1:])
		w.logger.Debug("cloudflared output", "line", strings.TrimSpace(line))
		if readyPattern.MatchString(line) {
			w.found = true
			select {
			case w.ready <- struct{}{}:
			default:
			}
			w.buf.Reset()
			break
		}
	}
	// Bound memory if cloudflared emits a huge marker-less line.
	if !w.found && w.buf.Len() > 64*1024 {
		w.buf.Reset()
	}
	return len(p), nil
}
