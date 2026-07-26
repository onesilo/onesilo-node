package lanserve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// maxFrameBytes bounds one inbound WebSocket frame; user_message payloads
// with long histories/attachments can be large.
const maxFrameBytes = 16 << 20

// maxClients caps concurrent WebSocket connections. The port is on 0.0.0.0,
// so without a cap any LAN host could open connections until the node runs
// out of fds/goroutines. Legitimate use is a handful of local clients.
const maxClients = 64

// idleReadTimeout closes a WebSocket that sends nothing for this long,
// reclaiming connections held open by idle or slow-loris clients. It is a
// per-read deadline, refreshed on every frame, so an active session with
// long think-time between messages stays connected.
const idleReadTimeout = 5 * time.Minute

// Server is the single LAN-facing HTTP server on lan.port:
//
//	/            WebSocket upgrade (the iOS LLM protocol, root path)
//	GET /healthz liveness probe
//	/v1/         node HTTP APIs (memory, gateway relay) when provided
type Server struct {
	port          int
	router        *Router
	apiHandler    http.Handler
	logger        *slog.Logger
	onClientCount func(int)

	clients atomic.Int64

	mu     sync.Mutex
	srv    *http.Server
	ln     net.Listener
	ctx    context.Context
	cancel context.CancelFunc
}

// NewServer builds the LAN server. apiHandler serves everything under /v1/
// (memory API, gateway relay) and may be nil; onClientCount may be nil.
func NewServer(port int, router *Router, apiHandler http.Handler, onClientCount func(int), logger *slog.Logger) *Server {
	return &Server{
		port:          port,
		router:        router,
		apiHandler:    apiHandler,
		logger:        logger,
		onClientCount: onClientCount,
	}
}

// Start binds 0.0.0.0:<port> and serves in a background goroutine until
// Stop or ctx cancellation.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return nil
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("binding LAN server on :%d: %w", s.port, err)
	}

	serveCtx, cancel := context.WithCancel(ctx)
	s.ctx = serveCtx
	s.cancel = cancel

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
	if s.apiHandler != nil {
		mux.Handle("/v1/", s.apiHandler)
	}
	mux.HandleFunc("/", s.handleWebSocket)

	// ReadHeaderTimeout stops slow-loris header trickling; IdleTimeout
	// reaps idle keep-alive connections on the HTTP (non-WebSocket) side.
	// No overall ReadTimeout/WriteTimeout: the WebSocket upgrade hijacks the
	// conn and manages its own deadlines (see handleWebSocket).
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.srv = srv
	s.ln = ln
	s.logger.Info("LAN server listening", "addr", ln.Addr().String())

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("LAN server failed", "error", err)
		}
	}()
	return nil
}

// Stop shuts the server down: closes the listener, cancels WebSocket
// sessions, and waits (bounded by ctx) for in-flight generations.
func (s *Server) Stop(ctx context.Context) {
	s.mu.Lock()
	srv := s.srv
	cancel := s.cancel
	s.srv = nil
	s.ln = nil
	s.cancel = nil
	s.mu.Unlock()
	if srv == nil {
		return
	}
	if cancel != nil {
		cancel() // unblocks WebSocket read loops (hijacked conns ignore Shutdown)
	}
	if err := srv.Shutdown(ctx); err != nil {
		srv.Close()
	}
	done := make(chan struct{})
	go func() { s.router.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Addr returns the bound listen address ("" when not running). Useful in
// tests where port 0 is used.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Running reports whether the server is currently listening.
func (s *Server) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srv != nil
}

// ClientCount returns the number of connected WebSocket clients.
func (s *Server) ClientCount() int { return int(s.clients.Load()) }

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// Reject over the cap before upgrading, so a flood can't exhaust fds and
	// goroutines. Reserve the slot atomically and release it on return.
	if n := s.clients.Add(1); n > maxClients {
		s.clients.Add(-1)
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	slotReleased := false
	releaseSlot := func() {
		if !slotReleased {
			slotReleased = true
			n := s.clients.Add(-1)
			s.notifyClients(int(n))
			s.logger.Info("LAN client disconnected", "remote", r.RemoteAddr, "clients", n)
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// LAN clients (URLSessionWebSocketTask) send no browser Origin;
		// origin checking is meaningless here — the payload is E2E encrypted.
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logger.Debug("websocket accept failed", "error", err)
		s.clients.Add(-1) // never counted as connected; release quietly
		return
	}
	conn.SetReadLimit(maxFrameBytes)

	s.mu.Lock()
	serveCtx := s.ctx
	s.mu.Unlock()
	if serveCtx == nil { // stopped between accept and here
		s.clients.Add(-1) // release the reserved slot (never went "connected")
		conn.Close(websocket.StatusGoingAway, "server stopping")
		return
	}
	// The connection lives until the client goes away or the server stops.
	ctx, cancel := context.WithCancel(serveCtx)
	defer cancel()
	stop := context.AfterFunc(r.Context(), cancel)
	defer stop()

	// The slot was reserved before Accept; publish the current count now.
	connected := int(s.clients.Load())
	s.notifyClients(connected)
	s.logger.Info("LAN client connected", "remote", r.RemoteAddr, "clients", connected)
	defer releaseSlot()

	sess := NewSession(func(ctx context.Context, data []byte) error {
		return conn.Write(ctx, websocket.MessageText, data)
	})
	defer sess.CancelGeneration()
	defer conn.CloseNow()

	// First frame: plain connected status (the iOS client skips it before
	// the health-check response) — matches SiloMac/SiloDesktop.
	if err := sess.sendJSON(ctx, newStatus("connected", "Connected to Silo LLM")); err != nil {
		return
	}

	for {
		// Per-read idle deadline: an active client refreshes it every frame;
		// a silent/slow-loris client is dropped after idleReadTimeout.
		readCtx, cancelRead := context.WithTimeout(ctx, idleReadTimeout)
		_, frame, err := conn.Read(readCtx)
		cancelRead()
		if err != nil {
			return
		}
		s.router.Handle(ctx, sess, frame)
	}
}

func (s *Server) notifyClients(n int) {
	if s.onClientCount != nil {
		s.onClientCount(n)
	}
}
