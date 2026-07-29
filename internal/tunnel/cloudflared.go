// Package tunnel manages a Cloudflare quick tunnel: it spawns
// `cloudflared tunnel --url http://localhost:<port>`, parses the ephemeral
// trycloudflare.com URL from the process output (cloudflared logs to
// stderr), and restarts with backoff on unexpected exit. Mirrors SiloMac's
// CloudflareTunnelService.
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

// ErrBinaryNotFound means no cloudflared install was located.
var ErrBinaryNotFound = errors.New("cloudflared binary not found (checked configured path, Homebrew paths and $PATH)")

// ErrURLTimeout means cloudflared started but never printed a tunnel URL.
var ErrURLTimeout = errors.New("timed out waiting for the tunnel URL")

// urlPattern matches the ephemeral quick-tunnel URL in cloudflared output.
var urlPattern = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)

// readyTimeout is how long we wait for the URL after spawning cloudflared.
const readyTimeout = 30 * time.Second

const (
	initialBackoff = time.Second
	maxBackoff     = time.Minute
)

// ExtractURL returns the trycloudflare URL contained in a cloudflared
// output chunk, if any.
func ExtractURL(output string) (string, bool) {
	m := urlPattern.FindString(output)
	return m, m != ""
}

// searchPaths are the well-known install locations tried before $PATH.
var searchPaths = []string{"/opt/homebrew/bin/cloudflared", "/usr/local/bin/cloudflared"}

// FindBinary locates cloudflared: the configured path first, then
// well-known Homebrew / local installs, then $PATH.
//
// A configured path that no longer resolves falls back to the search
// instead of failing. The Silo Mac app pins the copy inside its own bundle
// by absolute path, and that path goes stale whenever the app is rebuilt,
// moved, or updated — pinning the node to a binary that isn't there any
// more would keep the tunnel down while a perfectly good cloudflared sits
// on $PATH. Callers report the substitution with logFallback.
func FindBinary(configured string) (string, error) {
	if configured != "" && isExecutable(configured) {
		return configured, nil
	}
	for _, c := range searchPaths {
		if isExecutable(c) {
			return c, nil
		}
	}
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p, nil
	}
	if configured != "" {
		return "", fmt.Errorf("%w; the configured binary %s is missing or not executable",
			ErrBinaryNotFound, configured)
	}
	return "", ErrBinaryNotFound
}

// logFallback reports a configured cloudflared path FindBinary had to
// ignore. Logged at the point of use so the line says which copy the
// tunnel is actually running.
func logFallback(logger *slog.Logger, configured, used string) {
	if configured == "" || configured == used {
		return
	}
	logger.Warn("configured cloudflared binary is missing or not executable; "+
		"using the copy found on this system instead",
		"configured", configured, "using", used)
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// Manager runs one quick tunnel targeting a local port.
type Manager struct {
	port       int
	binaryPath string // optional configured override
	logger     *slog.Logger
	// onURLChange is called whenever the public URL changes, including with
	// "" when the tunnel goes down. Called from the manager goroutine.
	onURLChange func(string)

	mu      sync.Mutex
	url     string
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// New builds a manager. onURLChange may be nil.
func New(port int, binaryPath string, logger *slog.Logger, onURLChange func(string)) *Manager {
	return &Manager{port: port, binaryPath: binaryPath, logger: logger, onURLChange: onURLChange}
}

// URL returns the current public tunnel URL ("" while down).
func (m *Manager) URL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.url
}

// Start launches the tunnel loop. Idempotent. Fails fast when cloudflared
// is not installed; transient spawn failures are retried with backoff.
func (m *Manager) Start(ctx context.Context) error {
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
func (m *Manager) Stop() {
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

// runLoop keeps one cloudflared process alive, restarting with exponential
// backoff on unexpected exit. Each (re)start yields a fresh quick-tunnel
// URL, reported through onURLChange.
func (m *Manager) runLoop(ctx context.Context) {
	defer m.setURL("")
	backoff := initialBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		url, cmd, err := m.spawn(ctx)
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
		m.logger.Info("cloudflare tunnel ready", "url", url, "port", m.port)
		m.setURL(url)

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

// spawn starts cloudflared and waits (up to readyTimeout) for the tunnel
// URL to appear on stdout or stderr. On error the process is reaped.
func (m *Manager) spawn(ctx context.Context) (string, *exec.Cmd, error) {
	binary, err := FindBinary(m.binaryPath)
	if err != nil {
		return "", nil, err
	}

	logFallback(m.logger, m.binaryPath, binary)

	cmd := exec.Command(binary, "tunnel", "--url", fmt.Sprintf("http://localhost:%d", m.port))
	urlCh := make(chan string, 2)
	// Attach scanning writers instead of pipes so cmd.Wait needs no
	// coordination with readers and the process can never block on a full
	// pipe after we stop caring about output.
	cmd.Stdout = &urlScanWriter{urls: urlCh, logger: m.logger}
	cmd.Stderr = &urlScanWriter{urls: urlCh, logger: m.logger}

	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("starting cloudflared: %w", err)
	}
	m.logger.Info("cloudflared started", "binary", binary, "pid", cmd.Process.Pid)

	select {
	case url := <-urlCh:
		return url, cmd, nil
	case <-time.After(readyTimeout):
		m.reap(cmd)
		return "", nil, ErrURLTimeout
	case <-ctx.Done():
		m.reap(cmd)
		return "", nil, ctx.Err()
	}
}

func (m *Manager) reap(cmd *exec.Cmd) {
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

func (m *Manager) setURL(url string) {
	m.mu.Lock()
	changed := m.url != url
	m.url = url
	cb := m.onURLChange
	m.mu.Unlock()
	if changed && cb != nil {
		cb(url)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// urlScanWriter scans process output line-by-line for the tunnel URL and
// reports the first match. It keeps accepting (and discarding) output for
// the process's whole lifetime.
type urlScanWriter struct {
	urls   chan<- string
	logger *slog.Logger

	buf   strings.Builder
	found bool
}

func (w *urlScanWriter) Write(p []byte) (int, error) {
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
		if url, ok := ExtractURL(line); ok {
			w.found = true
			select {
			case w.urls <- url:
			default:
			}
			w.buf.Reset()
			break
		}
	}
	// Bound memory if cloudflared emits a huge URL-less line.
	if !w.found && w.buf.Len() > 64*1024 {
		w.buf.Reset()
	}
	return len(p), nil
}
