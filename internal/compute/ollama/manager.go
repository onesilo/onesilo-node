package ollama

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Settings is what the Manager needs from configuration. Read live via a
// getter so admin-API config changes apply without rebuilding the manager.
type Settings struct {
	Host         string // base URL, e.g. http://127.0.0.1:11434
	Manage       bool   // spawn `ollama serve` when unreachable
	BinaryPath   string // optional explicit binary path
	DefaultModel string // preferred model tag
}

// Well-known install locations checked after the configured path and $PATH.
var binaryCandidates = []string{
	"/opt/homebrew/bin/ollama",
	"/usr/local/bin/ollama",
}

// Manager implements connect-or-manage semantics for the local Ollama
// server: prefer an already-running server; when unreachable and Manage is
// true, spawn a user-installed `ollama serve` and wait for it to come up.
type Manager struct {
	settings func() Settings
	logger   *slog.Logger

	mu  sync.Mutex
	cmd *exec.Cmd
}

// NewManager returns a manager reading settings live from the getter.
func NewManager(settings func() Settings, logger *slog.Logger) *Manager {
	return &Manager{settings: settings, logger: logger}
}

// Client returns a client for the currently configured host.
func (m *Manager) Client() *Client {
	return NewClient(m.settings().Host)
}

// Reachable reports whether an Ollama server answers at the configured host.
func (m *Manager) Reachable(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := m.Client().Version(probeCtx)
	return err == nil
}

// EnsureRunning makes sure a server is reachable, spawning one if allowed.
func (m *Manager) EnsureRunning(ctx context.Context) error {
	if m.Reachable(ctx) {
		return nil
	}
	s := m.settings()
	if !s.Manage {
		return fmt.Errorf("ollama server not reachable at %s (ollama.manage is false, not spawning)", s.Host)
	}

	m.mu.Lock()
	if m.cmd == nil {
		binary, err := findBinary(s.BinaryPath)
		if err != nil {
			m.mu.Unlock()
			return err
		}
		hostPort, err := hostPortFromURL(s.Host)
		if err != nil {
			m.mu.Unlock()
			return err
		}
		cmd := exec.Command(binary, "serve")
		cmd.Env = append(os.Environ(), "OLLAMA_HOST="+hostPort)
		if err := cmd.Start(); err != nil {
			m.mu.Unlock()
			return fmt.Errorf("spawning %s serve: %w", binary, err)
		}
		m.cmd = cmd
		m.logger.Info("spawned ollama serve", "binary", binary, "pid", cmd.Process.Pid, "host", hostPort)
		go func() {
			err := cmd.Wait()
			m.mu.Lock()
			if m.cmd == cmd {
				m.cmd = nil
			}
			m.mu.Unlock()
			if err != nil {
				m.logger.Warn("ollama serve exited", "error", err)
			}
		}()
	}
	m.mu.Unlock()

	// Wait-ready loop: up to 20 x 250ms, mirroring the SiloMac runtime.
	for i := 0; i < 20; i++ {
		if m.Reachable(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("ollama server did not become ready at %s", s.Host)
}

// Stop terminates a spawned server, if any. Servers we merely connected to
// are left alone.
func (m *Manager) Stop() {
	m.mu.Lock()
	cmd := m.cmd
	m.cmd = nil
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	m.logger.Info("stopping managed ollama serve", "pid", cmd.Process.Pid)
	_ = cmd.Process.Signal(syscall.SIGTERM)
}

// findBinary locates the ollama binary: explicit config path, then $PATH,
// then well-known Homebrew / local installs.
func findBinary(configured string) (string, error) {
	if configured != "" {
		if isExecutable(configured) {
			return configured, nil
		}
		return "", fmt.Errorf("configured ollama binary %s is not executable", configured)
	}
	if p, err := exec.LookPath("ollama"); err == nil {
		return p, nil
	}
	for _, c := range binaryCandidates {
		if isExecutable(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("ollama binary not found (checked $PATH, %v)", binaryCandidates)
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// hostPortFromURL converts http://127.0.0.1:11434 into the host:port form
// OLLAMA_HOST expects.
func hostPortFromURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid ollama host URL %q", raw)
	}
	return u.Host, nil
}
