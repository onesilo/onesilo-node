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
	if err := json.Unmarshal([]byte(backlog[0].Line), &entry); err != nil {
		t.Fatalf("recorded line is not JSON: %v (%q)", err, backlog[0].Line)
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
	if err := json.Unmarshal([]byte(rec.Backlog()[0].Line), &entry); err != nil {
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
	var lines []string
	for _, r := range rec.Backlog() {
		lines = append(lines, r.Line)
	}
	got := strings.Join(lines, ",")
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
	if len(backlog) != 1 || !strings.Contains(backlog[0].Line, "important") {
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
	case rec := <-ch:
		if rec.Line != "after" {
			t.Fatalf("want the live line, got %q", rec.Line)
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
		case rec := <-ch:
			if strings.Contains(rec.Line, "fell behind") {
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
	// The channel must stay OPEN until cancel. A closed one would end the
	// SSE handler's loop immediately, so the console would report a dead
	// connection when the truth is simply that there is nothing to show.
	select {
	case <-ch:
		t.Fatal("a nil recorder's subscription must not close on its own")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	cancel() // idempotent, like the real one
	if _, open := <-ch; open {
		t.Fatal("cancel should close the subscription channel")
	}
	if rec.Subscribers() != 0 {
		t.Fatal("a nil recorder has no subscribers")
	}
}

func TestSequenceNumbersAreMonotonicAcrossBacklogAndLive(t *testing.T) {
	// Sequence numbers are what let a consumer that reads the backlog and
	// then subscribes tell replayed records from new ones. Identical text
	// cannot: logging the same message twice is not a duplicate.
	rec := NewRecorder(10)
	rec.Write([]byte("repeated\n"))
	rec.Write([]byte("repeated\n"))

	backlog := rec.Backlog()
	if len(backlog) != 2 {
		t.Fatalf("want both identical lines retained, got %d", len(backlog))
	}
	if backlog[0].Seq != 1 || backlog[1].Seq != 2 {
		t.Fatalf("want seqs 1,2 got %d,%d", backlog[0].Seq, backlog[1].Seq)
	}

	ch, cancel := rec.Subscribe(4)
	defer cancel()
	rec.Write([]byte("repeated\n"))
	select {
	case got := <-ch:
		if got.Seq != 3 {
			t.Fatalf("live record should continue the sequence, got %d", got.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber received nothing")
	}
}
