package adminapi

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onesilo/onesilo-node/internal/logging"
)

// fakeLogController layers LogController onto fakeController.
type fakeLogController struct {
	fakeController

	mu       sync.Mutex
	backlog  []logging.Record
	live     chan logging.Record
	seq      uint64
	canceled bool
}

// newFakeLogs numbers the backlog 1..n, as a real recorder would.
func newFakeLogs(backlog ...string) *fakeLogController {
	f := &fakeLogController{live: make(chan logging.Record, 8)}
	for _, line := range backlog {
		f.seq++
		f.backlog = append(f.backlog, logging.Record{Seq: f.seq, Line: line})
	}
	return f
}

// emit queues a live record continuing the sequence.
func (f *fakeLogController) emit(line string) {
	f.mu.Lock()
	f.seq++
	rec := logging.Record{Seq: f.seq, Line: line}
	f.mu.Unlock()
	f.live <- rec
}

// replay queues a record the backlog already carried, as the subscribe→
// backlog overlap produces.
func (f *fakeLogController) replay(seq uint64, line string) {
	f.live <- logging.Record{Seq: seq, Line: line}
}

func (f *fakeLogController) LogBacklog() []logging.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]logging.Record(nil), f.backlog...)
}

func (f *fakeLogController) SubscribeLogs() (<-chan logging.Record, func()) {
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

	ctrl.emit(`{"msg":"third"}`)
	expectLine(t, lines, `{"msg":"third"}`)
}

func TestLogStreamDoesNotDuplicateTheOverlap(t *testing.T) {
	// The handler subscribes before reading the backlog, so nothing written
	// in between is lost. The cost is that the first live records can repeat
	// the backlog's tail; the operator must not see the same line twice.
	ctrl := newFakeLogs(`{"msg":"a"}`, `{"msg":"b"}`)
	srv := httptest.NewServer(newMux("tok", ctrl, slog.Default()))
	defer srv.Close()

	// "b" was captured by both paths, as a real overlap would be: same
	// record, same sequence number, delivered twice.
	ctrl.replay(2, `{"msg":"b"}`)
	ctrl.emit(`{"msg":"c"}`)

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
	// An `id:` line plus one `data:` per line of the payload.
	if len(event) != 3 || event[0] != "id: 1" {
		t.Fatalf("want an id line and two data lines, got %v", event)
	}
	for _, line := range event[1:] {
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("a payload line escaped the data prefix: %q", line)
		}
	}
}

func TestLogStreamKeepsRepeatedIdenticalLines(t *testing.T) {
	// A program logging the same message twice is not a duplicate. Overlap
	// suppression keys off the sequence number precisely so identical text
	// cannot make a real line vanish from the live tail.
	ctrl := newFakeLogs(`{"msg":"reconcile"}`)
	srv := httptest.NewServer(newMux("tok", ctrl, slog.Default()))
	defer srv.Close()

	lines, stop := streamLines(t, srv, "tok")
	defer stop()
	expectLine(t, lines, `{"msg":"reconcile"}`)

	ctrl.emit(`{"msg":"reconcile"}`) // same text, later record
	expectLine(t, lines, `{"msg":"reconcile"}`)
	ctrl.emit(`{"msg":"reconcile"}`)
	expectLine(t, lines, `{"msg":"reconcile"}`)
}

func TestLogStreamStaysOpenWithNothingToShow(t *testing.T) {
	// A controller with no records must give a live-but-empty console, not
	// a stream that ends the moment it opens — those look identical to the
	// operator otherwise, and one of them reads as broken.
	ctrl := newFakeLogs()
	srv := httptest.NewServer(newMux("tok", ctrl, slog.Default()))
	defer srv.Close()

	lines, stop := streamLines(t, srv, "tok")
	defer stop()

	select {
	case _, open := <-lines:
		if !open {
			t.Fatal("the stream ended instead of waiting for records")
		}
	case <-time.After(200 * time.Millisecond):
		// Still open with nothing to say: correct.
	}

	ctrl.emit(`{"msg":"finally"}`)
	expectLine(t, lines, `{"msg":"finally"}`)
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

// failingWriter is a ResponseWriter whose body writes fail from the start,
// standing in for a connection the peer has already dropped.
type failingWriter struct {
	header http.Header
	status int
	writes int
}

func (f *failingWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}
func (f *failingWriter) WriteHeader(code int) { f.status = code }
func (f *failingWriter) Write(p []byte) (int, error) {
	f.writes++
	return 0, errors.New("connection reset by peer")
}
func (f *failingWriter) Flush() {}

func TestLogStreamGivesUpWhenWritesFail(t *testing.T) {
	// Request-context cancellation usually ends the handler, but a failed
	// write is the earlier and more direct signal. Ignoring it would leave
	// the loop writing into a dead connection until something else noticed.
	ctrl := newFakeLogs(`{"msg":"a"}`, `{"msg":"b"}`, `{"msg":"c"}`)
	mux := newMux("tok", ctrl, slog.Default())

	req := httptest.NewRequest("GET", "/v1/logs/stream", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := &failingWriter{}

	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler kept going after the connection died")
	}
	if w.writes != 1 {
		t.Fatalf("want the handler to stop on the first failed write, got %d writes", w.writes)
	}
	if !ctrl.wasCanceled() {
		t.Fatal("the subscription should be released when the handler exits")
	}
}
