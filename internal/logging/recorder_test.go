package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/onesilo/onesilo-node/internal/config"
)

func TestRecordingLoggerWritesBothPlaces(t *testing.T) {
	// The operator's configured output must be untouched by recording; the
	// recorder must get JSON regardless, so the UI parses one shape.
	var stderr bytes.Buffer
	rec := NewRecorder(10)
	logger := NewRecording(config.Log{Format: "text", Level: "info"}, &stderr, rec)

	logger.Info("admin API listening", "addr", "127.0.0.1:8766")

	if !strings.Contains(stderr.String(), "msg=\"admin API listening\"") {
		t.Fatalf("text output not preserved: %q", stderr.String())
	}
	backlog := rec.Backlog()
	if len(backlog) != 1 {
		t.Fatalf("want 1 recorded line, got %d", len(backlog))
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(backlog[0]), &entry); err != nil {
		t.Fatalf("recorded line is not JSON: %v (%q)", err, backlog[0])
	}
	if entry["msg"] != "admin API listening" || entry["addr"] != "127.0.0.1:8766" {
		t.Fatalf("recorded entry lost fields: %v", entry)
	}
}

func TestRecordingCarriesWithAttrs(t *testing.T) {
	// Capabilities log through logger.With("capability", ...). If the fanout
	// dropped that state, the console would show every capability's lines
	// with no way to tell them apart.
	var stderr bytes.Buffer
	rec := NewRecorder(10)
	logger := NewRecording(config.Log{Level: "info"}, &stderr, rec).
		With("capability", "memory").
		WithGroup("db")

	logger.Info("opened", "path", "/tmp/x")

	var entry map[string]any
	if err := json.Unmarshal([]byte(rec.Backlog()[0]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["capability"] != "memory" {
		t.Fatalf("With attrs lost: %v", entry)
	}
	group, _ := entry["db"].(map[string]any)
	if group["path"] != "/tmp/x" {
		t.Fatalf("WithGroup lost: %v", entry)
	}
}

func TestBacklogKeepsTheNewestLinesInOrder(t *testing.T) {
	rec := NewRecorder(3)
	for _, s := range []string{"one", "two", "three", "four", "five"} {
		rec.Write([]byte(s + "\n"))
	}
	got := strings.Join(rec.Backlog(), ",")
	if got != "three,four,five" {
		t.Fatalf("want the newest three in order, got %q", got)
	}
}

func TestLevelFilteringAppliesToTheRecorder(t *testing.T) {
	// The console shows what the node's configured level admits — a debug
	// line at level=warn is not written anywhere, so it cannot be recorded.
	var stderr bytes.Buffer
	rec := NewRecorder(10)
	logger := NewRecording(config.Log{Level: "warn"}, &stderr, rec)

	logger.Info("chatty")
	logger.Warn("important")

	backlog := rec.Backlog()
	if len(backlog) != 1 || !strings.Contains(backlog[0], "important") {
		t.Fatalf("want only the warning recorded, got %v", backlog)
	}
}

func TestSubscribersReceiveNewLinesOnly(t *testing.T) {
	rec := NewRecorder(10)
	rec.Write([]byte("before\n"))

	ch, cancel := rec.Subscribe(4)
	defer cancel()
	rec.Write([]byte("after\n"))

	select {
	case line := <-ch:
		if line != "after" {
			t.Fatalf("want the live line, got %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber received nothing")
	}
}

func TestCancelClosesTheChannelAndIsIdempotent(t *testing.T) {
	rec := NewRecorder(10)
	ch, cancel := rec.Subscribe(1)
	cancel()
	cancel() // a double cancel must not panic on a closed channel

	if _, open := <-ch; open {
		t.Fatal("cancel should close the subscription channel")
	}
	if rec.Subscribers() != 0 {
		t.Fatalf("subscription leaked: %d remaining", rec.Subscribers())
	}
}

func TestASlowSubscriberNeverBlocksTheLogger(t *testing.T) {
	// The whole point of the buffered fan-out: a console that stops reading
	// must not be able to wedge the node. It must also be told it missed
	// lines rather than silently shown a gap.
	rec := NewRecorder(100)
	ch, cancel := rec.Subscribe(2)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			rec.Write([]byte("line\n"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writing blocked on a subscriber that stopped reading")
	}

	// Drain and look for the confession.
	<-ch
	<-ch
	rec.Write([]byte("next\n"))
	var sawNotice bool
	for i := 0; i < 2; i++ {
		select {
		case line := <-ch:
			if strings.Contains(line, "fell behind") {
				sawNotice = true
			}
		case <-time.After(time.Second):
		}
	}
	if !sawNotice {
		t.Fatal("dropped lines were not reported to the subscriber")
	}
}

func TestNilRecorderIsUsable(t *testing.T) {
	// A node built without a recorder has an empty console, not a panic.
	var rec *Recorder
	if got := rec.Backlog(); got != nil {
		t.Fatalf("want nil backlog, got %v", got)
	}
	if _, err := rec.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	ch, cancel := rec.Subscribe(1)
	cancel()
	if _, open := <-ch; open {
		t.Fatal("a nil recorder should hand back a closed channel")
	}
	if rec.Subscribers() != 0 {
		t.Fatal("a nil recorder has no subscribers")
	}
}
