package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"

	"github.com/onesilo/onesilo-node/internal/config"
)

// DefaultRecorderCapacity is how many recent log lines the node keeps for
// the admin UI's console. Enough that opening the page mid-incident shows
// what led up to it, small enough to stay a rounding error in memory.
const DefaultRecorderCapacity = 1000

// Recorder keeps the most recent log records in memory and fans new ones
// out to live subscribers. It exists so the admin UI can show what the node
// is doing right now: in `onesilo-node setup`'s control panel the logs go to
// a file precisely so they don't scribble over the screen, which leaves the
// operator with no view of the node at all.
//
// Records arrive as complete JSON lines (slog writes one record per Write),
// so subscribers and the ring both deal in ready-to-send strings.
//
// Every method is safe on a nil *Recorder: a node built without one behaves
// like a node whose console is simply empty, rather than panicking.
type Recorder struct {
	mu       sync.Mutex
	ring     []string
	next     int // write cursor into ring
	filled   int // entries written, capped at len(ring)
	subs     map[int]*subscriber
	nextSubI int
}

type subscriber struct {
	ch chan string
	// dropped counts records skipped because ch was full. Reported to the
	// subscriber on the next successful send rather than swallowed — a
	// console with silent gaps is worse than one that admits to them.
	dropped int
}

// NewRecorder returns a recorder retaining the last `capacity` records.
func NewRecorder(capacity int) *Recorder {
	if capacity <= 0 {
		capacity = DefaultRecorderCapacity
	}
	return &Recorder{
		ring: make([]string, capacity),
		subs: map[int]*subscriber{},
	}
}

// Write records one log line. It satisfies io.Writer so a slog handler can
// target it directly; slog emits exactly one record per Write.
func (r *Recorder) Write(p []byte) (int, error) {
	if r == nil {
		return len(p), nil
	}
	line := string(bytes.TrimRight(p, "\n"))
	if line == "" {
		return len(p), nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.ring[r.next] = line
	r.next = (r.next + 1) % len(r.ring)
	if r.filled < len(r.ring) {
		r.filled++
	}
	for _, s := range r.subs {
		s.send(line)
	}
	return len(p), nil
}

// send delivers without ever blocking the logger: a stalled console must
// not be able to wedge the node.
func (s *subscriber) send(line string) {
	if s.dropped > 0 {
		// Try to confess the gap first; if that doesn't fit either, keep
		// counting and try again on the next record.
		notice, _ := json.Marshal(map[string]any{
			"level":   "WARN",
			"msg":     "log console fell behind; some lines were dropped",
			"dropped": s.dropped,
		})
		select {
		case s.ch <- string(notice):
			s.dropped = 0
		default:
			s.dropped++
			return
		}
	}
	select {
	case s.ch <- line:
	default:
		s.dropped++
	}
}

// Backlog returns the retained records, oldest first.
func (r *Recorder) Backlog() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, r.filled)
	start := 0
	if r.filled == len(r.ring) {
		start = r.next // ring is full: oldest sits at the write cursor
	}
	for i := 0; i < r.filled; i++ {
		out = append(out, r.ring[(start+i)%len(r.ring)])
	}
	return out
}

// Subscribe returns a channel of records written from now on, plus a cancel
// function. Cancel must be called (defer it) or the subscription leaks.
// Records that don't fit in the buffer are dropped, not blocked on.
func (r *Recorder) Subscribe(buffer int) (<-chan string, func()) {
	if r == nil {
		ch := make(chan string)
		close(ch)
		return ch, func() {}
	}
	if buffer <= 0 {
		buffer = 256
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextSubI
	r.nextSubI++
	sub := &subscriber{ch: make(chan string, buffer)}
	r.subs[id] = sub
	return sub.ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(sub.ch)
		}
	}
}

// Subscribers reports how many live subscriptions exist (tests).
func (r *Recorder) Subscribers() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subs)
}

// NewRecording is NewWithWriter plus a recorder: records go to w in the
// configured format AND to rec as JSON. The recorder is always fed JSON so
// the admin UI parses one shape regardless of how the operator configured
// their own log output.
func NewRecording(cfg config.Log, w io.Writer, rec *Recorder) *slog.Logger {
	opts := handlerOptions(cfg)
	return slog.New(fanout{
		handlerFor(cfg, w, opts),
		slog.NewJSONHandler(rec, opts),
	})
}

// fanout sends every record to each handler. Attr/group state is applied to
// all of them so the recorded copy carries the same context as the written
// one.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, rec slog.Record) error {
	var firstErr error
	for _, h := range f {
		if !h.Enabled(ctx, rec.Level) {
			continue
		}
		// Each handler consumes the record's attrs, so hand out clones.
		if err := h.Handle(ctx, rec.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}
