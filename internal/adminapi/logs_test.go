package adminapi

import (
	"bufio"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLogController layers LogController onto fakeController.
type fakeLogController struct {
	fakeController

	mu       sync.Mutex
	backlog  []string
	live     chan string
	canceled bool
}

func newFakeLogs(backlog ...string) *fakeLogController {
	return &fakeLogController{backlog: backlog, live: make(chan string, 8)}
}

func (f *fakeLogController) LogBacklog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.backlog...)
}

func (f *fakeLogController) SubscribeLogs() (<-chan string, func()) {
	return f.live, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.canceled = true
	}
}

func (f *fakeLogController) wasCanceled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canceled
}

// streamLines opens the SSE endpoint and returns a channel of `data:`
// payloads plus a stop function.
func streamLines(t *testing.T, srv *httptest.Server, token string) (<-chan string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/logs/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		cancel()
		t.Fatalf("stream returned %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		res.Body.Close()
		cancel()
		t.Fatalf("want an SSE content type, got %q", ct)
	}
	out := make(chan string, 32)
	go func() {
		defer close(out)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				out <- data
			}
		}
	}()
	return out, func() { cancel(); res.Body.Close() }
}

func expectLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	select {
	case got := <-lines:
		if got != want {
			t.Fatalf("want %q, got %q", want, got)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

func TestLogStreamSendsBacklogThenLive(t *testing.T) {
	// Opening the console mid-incident has to show what led up to it, then
	// keep going — one request, history and live in the same stream.
	ctrl := newFakeLogs(`{"msg":"first"}`, `{"msg":"second"}`)
	srv := httptest.NewServer(newMux("tok", ctrl, slog.Default()))
	defer srv.Close()

	lines, stop := streamLines(t, srv, "tok")
	defer stop()

	expectLine(t, lines, `{"msg":"first"}`)
	expectLine(t, lines, `{"msg":"second"}`)

	ctrl.live <- `{"msg":"third"}`
	expectLine(t, lines, `{"msg":"third"}`)
}

func TestLogStreamDoesNotDuplicateTheOverlap(t *testing.T) {
	// The handler subscribes before reading the backlog, so nothing written
	// in between is lost. The cost is that the first live records can repeat
	// the backlog's tail; the operator must not see the same line twice.
	ctrl := newFakeLogs(`{"msg":"a"}`, `{"msg":"b"}`)
	srv := httptest.NewServer(newMux("tok", ctrl, slog.Default()))
	defer srv.Close()

	// "b" was captured by both paths, as a real overlap would be.
	ctrl.live <- `{"msg":"b"}`
	ctrl.live <- `{"msg":"c"}`

	lines, stop := streamLines(t, srv, "tok")
	defer stop()

	expectLine(t, lines, `{"msg":"a"}`)
	expectLine(t, lines, `{"msg":"b"}`)
	expectLine(t, lines, `{"msg":"c"}`) // the echoed "b" was dropped, not shown twice
}

func TestLogStreamRequiresTheAdminToken(t *testing.T) {
	ctrl := newFakeLogs(`{"msg":"secret"}`)
	srv := httptest.NewServer(newMux("tok", ctrl, slog.Default()))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/v1/logs/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without a token, got %d", res.StatusCode)
	}
}

func TestLogStreamUnsubscribesWhenTheClientLeaves(t *testing.T) {
	// Every open console holds a subscription; leaking them on disconnect
	// would grow the fan-out for the life of the process.
	ctrl := newFakeLogs()
	srv := httptest.NewServer(newMux("tok", ctrl, slog.Default()))
	defer srv.Close()

	_, stop := streamLines(t, srv, "tok")
	stop()

	deadline := time.Now().Add(3 * time.Second)
	for !ctrl.wasCanceled() {
		if time.Now().After(deadline) {
			t.Fatal("subscription was not cancelled after the client disconnected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLogStreamCannotBeSplitByANewlineInARecord(t *testing.T) {
	// A payload carrying a newline must not be able to terminate the SSE
	// event early and forge a second one. Every line gets its own `data:`,
	// which the client rejoins.
	//
	// slog's JSON handler escapes newlines, so this cannot come from the
	// node's own logger today — the guard is for the contract, which says
	// records are strings and never enforces that they are JSON.
	ctrl := newFakeLogs("{\"msg\":\"line one\nline two\"}")
	srv := httptest.NewServer(newMux("tok", ctrl, slog.Default()))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/logs/stream", nil)
	req.Header.Set("Authorization", "Bearer tok")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// Read the first event (terminated by a blank line).
	sc := bufio.NewScanner(res.Body)
	var event []string
	for sc.Scan() {
		if sc.Text() == "" {
			break
		}
		event = append(event, sc.Text())
	}
	if len(event) != 2 {
		t.Fatalf("want the record split across two data lines, got %v", event)
	}
	for _, line := range event {
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("a payload line escaped the data prefix: %q", line)
		}
	}
}

func TestLogRoutesAbsentWithoutALogController(t *testing.T) {
	// A controller that can't supply logs simply has no endpoint, rather
	// than one that returns an empty stream forever.
	srv := httptest.NewServer(newMux("tok", &fakeController{}, slog.Default()))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/logs/stream", nil)
	req.Header.Set("Authorization", "Bearer tok")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 when logs are unavailable, got %d", res.StatusCode)
	}
}
