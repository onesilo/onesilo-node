package node

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

// capturingHandler records every emitted record so a test can assert the
// level a message came out at. Enabled for all levels — the point is to see
// the debug lines the suppression path produces.
type capturingHandler struct {
	levels   []slog.Level
	messages []string
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.levels = append(h.levels, r.Level)
	h.messages = append(h.messages, r.Message)
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func newLoggingNode() (*Node, *capturingHandler) {
	h := &capturingHandler{}
	return &Node{logger: slog.New(h), startFailures: map[string]string{}}, h
}

// Reconcile retries every 30s, so a standing cause has to stop repeating —
// but a *different* cause is news and must surface.
func TestStartFailureWarnsOnceThenOnChange(t *testing.T) {
	n, h := newLoggingNode()
	const key, msg = "capability:compute", "capability start failed, will retry"

	sameCause := errors.New("ollama not installed")
	n.noteStartFailure(key, msg, sameCause)
	n.noteStartFailure(key, msg, errors.New("ollama not installed")) // equal text
	n.noteStartFailure(key, msg, errors.New("connection refused"))   // new cause
	n.noteStartFailure(key, msg, errors.New("connection refused"))

	want := []slog.Level{slog.LevelWarn, slog.LevelDebug, slog.LevelWarn, slog.LevelDebug}
	if len(h.levels) != len(want) {
		t.Fatalf("got %d records, want %d", len(h.levels), len(want))
	}
	for i, level := range want {
		if h.levels[i] != level {
			t.Errorf("record %d logged at %v, want %v", i, h.levels[i], level)
		}
	}
}

// Failures from separate things never suppress each other.
func TestStartFailureTracksKeysIndependently(t *testing.T) {
	n, h := newLoggingNode()
	boom := errors.New("no cloudflared on this machine")

	n.noteStartFailure("tunnel:quick", "tunnel start failed, will retry", boom)
	n.noteStartFailure("tunnel:managed", "managed tunnel start failed, will retry", boom)

	for i, level := range h.levels {
		if level != slog.LevelWarn {
			t.Errorf("record %d (%q) logged at %v, want Warn", i, h.messages[i], level)
		}
	}
}

// Turning something off ends its retry loop. Switching it back on later has
// to warn again on the first failure rather than land in the suppression
// path — that's what the Reconcile-side clears are for.
func TestClearStartFailureRestoresTheWarning(t *testing.T) {
	n, h := newLoggingNode()
	const key, msg = "tunnel:managed", "managed tunnel start failed, will retry"
	boom := errors.New("no cloudflared on this machine")

	n.noteStartFailure(key, msg, boom)
	if !n.retryingStart(key) {
		t.Error("a recorded failure should read as retrying")
	}

	n.clearStartFailure(key)
	if n.retryingStart(key) {
		t.Error("a cleared failure should not read as retrying")
	}

	n.noteStartFailure(key, msg, boom) // same cause, but after a clear
	want := []slog.Level{slog.LevelWarn, slog.LevelWarn}
	if len(h.levels) != len(want) {
		t.Fatalf("got %d records, want %d", len(h.levels), len(want))
	}
	for i, level := range want {
		if h.levels[i] != level {
			t.Errorf("record %d logged at %v, want %v", i, h.levels[i], level)
		}
	}
}
