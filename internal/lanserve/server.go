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

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
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

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// LAN clients (URLSessionWebSocketTask) send no browser Origin;
		// origin checking is meaningless here — the payload is E2E encrypted.
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logger.Debug("websocket accept failed", "error", err)
		return
	}
	conn.SetReadLimit(maxFrameBytes)

	s.mu.Lock()
	serveCtx := s.ctx
	s.mu.Unlock()
	if serveCtx == nil { // stopped between accept and here
		conn.Close(websocket.StatusGoingAway, "server stopping")
		return
	}
	// The connection lives until the client goes away or the server stops.
	ctx, cancel := context.WithCancel(serveCtx)
	defer cancel()
	stop := context.AfterFunc(r.Context(), cancel)
	defer stop()

	n := s.clients.Add(1)
	s.notifyClients(int(n))
	s.logger.Info("LAN client connected", "remote", r.RemoteAddr, "clients", n)
	defer func() {
		n := s.clients.Add(-1)
		s.notifyClients(int(n))
		s.logger.Info("LAN client disconnected", "remote", r.RemoteAddr, "clients", n)
	}()

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
		_, frame, err := conn.Read(ctx)
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
