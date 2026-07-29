package lanserve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/onesilo/onesilo-node/internal/config"
)

func startTestServer(t *testing.T, memoryHandler http.Handler, onClients func(int)) *Server {
	t.Helper()
	r := NewRouter(func() []byte { return nil }, nil, slog.New(slog.DiscardHandler))
	s := NewServer(0, r, memoryHandler, onClients, slog.New(slog.DiscardHandler))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Stop(ctx)
	})
	return s
}

func TestServerWebSocketConnectedStatusAndHealthCheck(t *testing.T) {
	var mu sync.Mutex
	var counts []int
	s := startTestServer(t, nil, func(n int) {
		mu.Lock()
		counts = append(counts, n)
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws://"+s.Addr()+"/", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// First frame must be the plain connected status.
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read connected status: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("want text frame, got %v", typ)
	}
	var status map[string]any
	json.Unmarshal(data, &status)
	if status["type"] != "status" || status["status"] != "connected" || status["message"] != "Connected to Silo LLM" {
		t.Fatalf("unexpected first frame: %s", data)
	}

	// Plain health check round trip through the real server.
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"health_check","content":"abc"}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	json.Unmarshal(data, &resp)
	if resp["type"] != "health_check_response" || resp["content"] != "pong: abc" {
		t.Fatalf("unexpected health check response: %s", data)
	}

	if s.ClientCount() != 1 {
		t.Fatalf("client count = %d, want 1", s.ClientCount())
	}
	conn.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return s.ClientCount() == 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(counts) < 2 || counts[0] != 1 || counts[len(counts)-1] != 0 {
		t.Fatalf("client count callbacks: %v", counts)
	}
}

func TestServerHealthzAndMemoryMountAndNotFound(t *testing.T) {
	memory := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "memory-ok")
	})
	s := startTestServer(t, memory, nil)
	base := "http://" + s.Addr()

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("healthz: %d %s", resp.StatusCode, body)
	}

	resp, err = http.Get(base + "/v1/memory/silos")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "memory-ok" {
		t.Fatalf("memory mount: %d %s", resp.StatusCode, body)
	}

	// Non-root, non-mounted path is 404, not a WS upgrade.
	resp, err = http.Get(base + "/other")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /other = %d, want 404", resp.StatusCode)
	}
}

func TestServerStopClosesClients(t *testing.T) {
	r := NewRouter(func() []byte { return nil }, nil, slog.New(slog.DiscardHandler))
	s := NewServer(0, r, nil, nil, slog.New(slog.DiscardHandler))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+s.Addr()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil { // connected status
		t.Fatal(err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	s.Stop(stopCtx)

	// The client read must fail promptly after Stop.
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	if _, _, err := conn.Read(readCtx); err == nil {
		t.Fatal("read succeeded after server stop")
	}
	if s.Running() {
		t.Fatal("server still running after Stop")
	}
}

// --- capability + fake announcer ---

type fakeAnnouncer struct {
	mu        sync.Mutex
	announced bool
	instance  string
	port      int
	txt       []string
	updates   [][]string
}

func (f *fakeAnnouncer) Announce(instance string, port int, txt []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.announced = true
	f.instance = instance
	f.port = port
	f.txt = txt
	return nil
}

func (f *fakeAnnouncer) Update(txt []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, txt)
	f.txt = txt
	return nil
}

func (f *fakeAnnouncer) Shutdown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.announced = false
}

func TestCapabilityLifecycleAndBonjour(t *testing.T) {
	cfg := config.Default()
	cfg.LAN.Enabled = true
	cfg.LAN.Port = 0 // ephemeral for tests

	model := "llama3.2:3b"
	var mu sync.Mutex
	ann := &fakeAnnouncer{}

	cap := NewCapability(
		func() config.Config { return cfg },
		nil,
		nil,
		func() string { mu.Lock(); defer mu.Unlock(); return model },
		func() string { return "" },
		func() []byte { return nil },
		func() Announcer { return ann },
		slog.New(slog.DiscardHandler),
	)

	if !cap.Enabled() {
		t.Fatal("capability should be enabled with lan.enabled")
	}
	if err := cap.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	healthy, detail := cap.Healthy(context.Background())
	if !healthy {
		t.Fatalf("unhealthy after start: %s", detail)
	}
	if !cap.Published() {
		t.Fatal("bonjour not published")
	}
	ann.mu.Lock()
	if !strings.HasSuffix(ann.instance, " Silo LLM") {
		t.Fatalf("instance name %q must end with ' Silo LLM'", ann.instance)
	}
	wantTXT := []string{"model=llama3.2:3b", "version=1.0", "capabilities=chat,stream,e2e", "protocol=1"}
	if fmt.Sprint(ann.txt) != fmt.Sprint(wantTXT) {
		t.Fatalf("TXT = %v, want %v", ann.txt, wantTXT)
	}
	ann.mu.Unlock()

	// Model change → TXT re-announce (driven directly, not via the ticker).
	mu.Lock()
	model = "qwen2.5:7b"
	mu.Unlock()
	cap.mu.Lock()
	cap.reconcileBonjourLocked()
	cap.mu.Unlock()
	ann.mu.Lock()
	if len(ann.updates) != 1 || ann.updates[0][0] != "model=qwen2.5:7b" {
		t.Fatalf("updates = %v", ann.updates)
	}
	ann.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := cap.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if cap.Published() {
		t.Fatal("still published after Stop")
	}
	if healthy, _ := cap.Healthy(context.Background()); healthy {
		t.Fatal("healthy after Stop")
	}
}

func TestCapabilityEnabledByMemoryWithoutBonjour(t *testing.T) {
	cfg := config.Default()
	cfg.LAN.Enabled = false
	cfg.Capabilities.Memory = true
	cfg.LAN.Port = 0

	ann := &fakeAnnouncer{}
	cap := NewCapability(
		func() config.Config { return cfg },
		nil, nil,
		func() string { return "" },
		func() string { return "" },
		func() []byte { return nil },
		func() Announcer { return ann },
		slog.New(slog.DiscardHandler),
	)
	if !cap.Enabled() {
		t.Fatal("memory alone must enable the LAN server")
	}
	if err := cap.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cap.Stop(ctx)
	}()
	if cap.Published() {
		t.Fatal("memory-only node must not publish the LLM bonjour service")
	}
}
