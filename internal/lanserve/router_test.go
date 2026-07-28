package lanserve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/onesilo/onesilo-node/internal/compute/ollama"
)

// --- test plumbing ---

type frameCapture struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *frameCapture) write(_ context.Context, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, append([]byte(nil), data...))
	return nil
}

func (c *frameCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

func (c *frameCapture) frame(i int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frames[i]
}

// decoded is one outbound frame, decrypted when it was an envelope.
type decoded struct {
	encrypted bool
	userID    string
	msg       map[string]any
}

func (c *frameCapture) decodeAll(t *testing.T, key []byte) []decoded {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]decoded, 0, len(c.frames))
	for _, f := range c.frames {
		var env Envelope
		var probe struct {
			Content *string `json:"content"`
			UserID  *string `json:"user_id"`
		}
		if json.Unmarshal(f, &probe) == nil && probe.Content != nil && probe.UserID != nil {
			json.Unmarshal(f, &env)
			combined, err := base64.StdEncoding.DecodeString(env.Content)
			if err != nil {
				t.Fatalf("outbound envelope has bad base64: %v", err)
			}
			pt, err := Open(key, combined)
			if err != nil {
				t.Fatalf("cannot decrypt outbound envelope: %v", err)
			}
			var m map[string]any
			json.Unmarshal(pt, &m)
			out = append(out, decoded{encrypted: true, userID: env.UserID, msg: m})
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(f, &m); err != nil {
			t.Fatalf("unparseable outbound frame %q: %v", f, err)
		}
		out = append(out, decoded{msg: m})
	}
	return out
}

type fakeCompute struct {
	enabled bool
	genErr  error
	// stream builds the delta channel per call; ctx is the generation ctx.
	stream func(ctx context.Context) <-chan ollama.GenerateResult
}

func (f *fakeCompute) Enabled() bool { return f.enabled }

func (f *fakeCompute) StreamGenerate(ctx context.Context, prompt string, temperature float64) (<-chan ollama.GenerateResult, error) {
	if f.genErr != nil {
		return nil, f.genErr
	}
	return f.stream(ctx), nil
}

// scriptedStream yields the given tokens then a done chunk with counts.
func scriptedStream(tokens []string, promptTokens int) func(ctx context.Context) <-chan ollama.GenerateResult {
	return func(ctx context.Context) <-chan ollama.GenerateResult {
		out := make(chan ollama.GenerateResult)
		go func() {
			defer close(out)
			for _, tok := range tokens {
				select {
				case out <- ollama.GenerateResult{Delta: ollama.GenerateDelta{Response: tok}}:
				case <-ctx.Done():
					return
				}
			}
			select {
			case out <- ollama.GenerateResult{Delta: ollama.GenerateDelta{Done: true, PromptEvalCount: promptTokens, EvalCount: len(tokens)}}:
			case <-ctx.Done():
			}
		}()
		return out
	}
}

func testRouter(compute ComputeBackend, key []byte) (*Router, *Session, *frameCapture) {
	cap := &frameCapture{}
	keyFn := func() []byte { return key }
	if key == nil {
		keyFn = func() []byte { return nil }
	}
	r := NewRouter(keyFn, compute, slog.New(slog.DiscardHandler))
	return r, NewSession(cap.write), cap
}

func encryptInner(t *testing.T, key []byte, userID string, inner any) []byte {
	t.Helper()
	pt, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	combined, err := Seal(key, pt)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(Envelope{Content: base64.StdEncoding.EncodeToString(combined), UserID: userID})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timeout waiting for condition")
}

// --- plain (unencrypted) paths ---

func TestPlainHealthCheckPassthrough(t *testing.T) {
	r, s, cap := testRouter(nil, nil) // works with no pairing key at all
	r.Handle(context.Background(), s, []byte(`{"type":"health_check","content":"hi"}`))

	msgs := cap.decodeAll(t, nil)
	if len(msgs) != 1 || msgs[0].encrypted {
		t.Fatalf("want 1 plain frame, got %+v", msgs)
	}
	if msgs[0].msg["type"] != "health_check_response" || msgs[0].msg["content"] != "pong: hi" {
		t.Fatalf("unexpected response: %v", msgs[0].msg)
	}
}

func TestPlainHealthCheckNonStringContent(t *testing.T) {
	// Swift: json["content"] as? String ?? "" → "pong: ".
	r, s, cap := testRouter(nil, nil)
	r.Handle(context.Background(), s, []byte(`{"type":"health_check","content":42}`))
	msgs := cap.decodeAll(t, nil)
	if msgs[0].msg["content"] != "pong: " {
		t.Fatalf("unexpected response: %v", msgs[0].msg)
	}
}

func TestPlainUnknownIsInvalidFormat(t *testing.T) {
	r, s, cap := testRouter(nil, nil)
	for _, frame := range []string{`{"type":"user_message","content":"hi"}`, `not json`, `{"content":"x"}`} {
		cap.frames = nil
		r.Handle(context.Background(), s, []byte(frame))
		msgs := cap.decodeAll(t, nil)
		if len(msgs) != 1 || msgs[0].msg["type"] != "error" || msgs[0].msg["code"] != CodeInvalidFormat {
			t.Fatalf("frame %q: want plain INVALID_FORMAT error, got %+v", frame, msgs)
		}
		if msgs[0].msg["recoverable"] != true {
			t.Fatalf("error must be recoverable: %v", msgs[0].msg)
		}
	}
}

// --- envelope error paths ---

func TestEnvelopeWithoutKeyIsNoKey(t *testing.T) {
	r, s, cap := testRouter(nil, nil)
	r.Handle(context.Background(), s, []byte(`{"content":"aGVsbG8=","user_id":"user_1"}`))
	msgs := cap.decodeAll(t, nil)
	if len(msgs) != 1 || msgs[0].msg["code"] != CodeNoKey {
		t.Fatalf("want NO_KEY, got %+v", msgs)
	}
}

func TestEnvelopeBadBase64(t *testing.T) {
	r, s, cap := testRouter(nil, goldenKey(t))
	r.Handle(context.Background(), s, []byte(`{"content":"!!not-base64!!","user_id":"user_1"}`))
	msgs := cap.decodeAll(t, goldenKey(t))
	if len(msgs) != 1 || msgs[0].msg["code"] != CodeInvalidBase64 {
		t.Fatalf("want INVALID_BASE64, got %+v", msgs)
	}
}

func TestEnvelopeDecryptionFailed(t *testing.T) {
	key := goldenKey(t)
	otherKey := append([]byte(nil), key...)
	otherKey[0] ^= 0xff

	frame := encryptInner(t, otherKey, "user_1", HealthCheckRequest{Type: "health_check", Content: "x"})
	r, s, cap := testRouter(nil, key)
	r.Handle(context.Background(), s, frame)
	msgs := cap.decodeAll(t, key)
	if len(msgs) != 1 || msgs[0].msg["code"] != CodeDecryptionFailed {
		t.Fatalf("want DECRYPTION_FAILED, got %+v", msgs)
	}
}

func TestInnerMissingTypeAndUnknownType(t *testing.T) {
	key := goldenKey(t)
	r, s, cap := testRouter(nil, key)

	r.Handle(context.Background(), s, encryptInner(t, key, "u", map[string]any{"content": "no type"}))
	msgs := cap.decodeAll(t, key)
	if len(msgs) != 1 || msgs[0].encrypted || msgs[0].msg["error"] != "Missing message type in decrypted message" {
		t.Fatalf("want plain missing-type error, got %+v", msgs)
	}

	cap.frames = nil
	r.Handle(context.Background(), s, encryptInner(t, key, "u", map[string]any{"type": "bogus"}))
	msgs = cap.decodeAll(t, key)
	if len(msgs) != 1 || msgs[0].encrypted || msgs[0].msg["error"] != "Unknown message type: bogus" {
		t.Fatalf("want plain unknown-type error, got %+v", msgs)
	}
}

// --- encrypted flows ---

func TestEncryptedHealthCheck(t *testing.T) {
	key := goldenKey(t)
	r, s, cap := testRouter(nil, key)
	r.Handle(context.Background(), s, encryptInner(t, key, "user_42", HealthCheckRequest{Type: "health_check", Content: "ping-abc"}))

	msgs := cap.decodeAll(t, key)
	if len(msgs) != 1 || !msgs[0].encrypted {
		t.Fatalf("want 1 encrypted frame, got %+v", msgs)
	}
	if msgs[0].userID != "user_42" {
		t.Fatalf("user_id not echoed: %q", msgs[0].userID)
	}
	if msgs[0].msg["type"] != "health_check_response" || msgs[0].msg["content"] != "pong: ping-abc" {
		t.Fatalf("unexpected response: %v", msgs[0].msg)
	}
}

func TestUserMessageStreamsDeltasMetadataCompleted(t *testing.T) {
	key := goldenKey(t)
	compute := &fakeCompute{enabled: true, stream: scriptedStream([]string{"Hel", "lo", "!"}, 7)}
	r, s, cap := testRouter(compute, key)

	temp := 0.5
	r.Handle(context.Background(), s, encryptInner(t, key, "user_9", UserMessage{Type: "user_message", Content: "hi", Temperature: &temp}))
	r.Wait()

	msgs := cap.decodeAll(t, key)
	// processing, generating, 3 deltas, metadata, completed = 7 frames
	if len(msgs) != 7 {
		t.Fatalf("want 7 frames, got %d: %+v", len(msgs), msgs)
	}
	for i, m := range msgs {
		if !m.encrypted || m.userID != "user_9" {
			t.Fatalf("frame %d not encrypted / wrong user: %+v", i, m)
		}
	}
	if msgs[0].msg["status"] != "processing" || msgs[1].msg["status"] != "generating" {
		t.Fatalf("unexpected status frames: %v %v", msgs[0].msg, msgs[1].msg)
	}
	for i, want := range []string{"Hel", "lo", "!"} {
		m := msgs[2+i].msg
		if m["type"] != "content_delta" || m["delta"] != want || m["index"] != float64(i) {
			t.Fatalf("delta %d: %v", i, m)
		}
	}
	meta := msgs[5].msg
	if meta["type"] != "llm_response_metadata" ||
		meta["total_input_tokens"] != float64(7) ||
		meta["total_output_tokens"] != float64(3) {
		t.Fatalf("metadata: %v", meta)
	}
	for _, k := range []string{"thinking_time_seconds", "average_tokens_per_second"} {
		if _, ok := meta[k].(float64); !ok {
			t.Fatalf("metadata missing %s: %v", k, meta)
		}
	}
	done := msgs[6].msg
	if done["type"] != "completed" || done["finish_reason"] != "stop" {
		t.Fatalf("completed: %v", done)
	}
	if _, present := done["usage"]; present {
		t.Fatalf("usage must be omitted: %v", done)
	}
}

func TestInterruptCancelsMidStream(t *testing.T) {
	key := goldenKey(t)
	// Stream: one token, then block until the generation ctx is cancelled.
	compute := &fakeCompute{enabled: true, stream: func(ctx context.Context) <-chan ollama.GenerateResult {
		out := make(chan ollama.GenerateResult)
		go func() {
			defer close(out)
			out <- ollama.GenerateResult{Delta: ollama.GenerateDelta{Response: "first"}}
			<-ctx.Done() // interrupt lands here; channel closes without Done
		}()
		return out
	}}
	r, s, cap := testRouter(compute, key)

	r.Handle(context.Background(), s, encryptInner(t, key, "u", UserMessage{Type: "user_message", Content: "hi"}))
	// processing, generating, first delta
	waitFor(t, func() bool { return cap.count() >= 3 })

	r.Handle(context.Background(), s, encryptInner(t, key, "u", InterruptMessage{Type: "interrupt"}))
	r.Wait()

	msgs := cap.decodeAll(t, key)
	// processing, generating, delta, metadata, completed — SiloMac still
	// sends metadata + completed after an interrupt.
	if len(msgs) != 5 {
		t.Fatalf("want 5 frames, got %d: %+v", len(msgs), msgs)
	}
	if msgs[2].msg["type"] != "content_delta" || msgs[2].msg["delta"] != "first" {
		t.Fatalf("delta: %v", msgs[2].msg)
	}
	if msgs[3].msg["type"] != "llm_response_metadata" || msgs[3].msg["total_output_tokens"] != float64(1) {
		t.Fatalf("metadata after interrupt: %v", msgs[3].msg)
	}
	if msgs[4].msg["type"] != "completed" || msgs[4].msg["finish_reason"] != "stop" {
		t.Fatalf("completed after interrupt: %v", msgs[4].msg)
	}
}

func TestStopCancelsLikeInterrupt(t *testing.T) {
	key := goldenKey(t)
	compute := &fakeCompute{enabled: true, stream: func(ctx context.Context) <-chan ollama.GenerateResult {
		out := make(chan ollama.GenerateResult)
		go func() {
			defer close(out)
			<-ctx.Done()
		}()
		return out
	}}
	r, s, cap := testRouter(compute, key)
	r.Handle(context.Background(), s, encryptInner(t, key, "u", UserMessage{Type: "user_message", Content: "hi"}))
	waitFor(t, func() bool { return cap.count() >= 2 })
	r.Handle(context.Background(), s, encryptInner(t, key, "u", StopMessage{Type: "stop"}))
	r.Wait()
	msgs := cap.decodeAll(t, key)
	last := msgs[len(msgs)-1].msg
	if last["type"] != "completed" {
		t.Fatalf("want completed last, got %v", last)
	}
}

func TestUserMessageComputeDisabled(t *testing.T) {
	key := goldenKey(t)
	for name, compute := range map[string]ComputeBackend{
		"nil-backend": nil,
		"disabled":    &fakeCompute{enabled: false},
	} {
		r, s, cap := testRouter(compute, key)
		r.Handle(context.Background(), s, encryptInner(t, key, "u", UserMessage{Type: "user_message", Content: "hi"}))
		r.Wait()
		msgs := cap.decodeAll(t, key)
		// processing status, then encrypted OLLAMA_UNAVAILABLE error.
		if len(msgs) != 2 {
			t.Fatalf("%s: want 2 frames, got %+v", name, msgs)
		}
		last := msgs[1]
		if !last.encrypted || last.msg["type"] != "error" || last.msg["code"] != CodeOllamaUnavailable {
			t.Fatalf("%s: want encrypted OLLAMA_UNAVAILABLE, got %+v", name, last)
		}
		if last.msg["recoverable"] != true {
			t.Fatalf("%s: error must be recoverable", name)
		}
	}
}

func TestUserMessageStreamStartError(t *testing.T) {
	key := goldenKey(t)
	compute := &fakeCompute{enabled: true, genErr: errors.New("no models installed")}
	r, s, cap := testRouter(compute, key)
	r.Handle(context.Background(), s, encryptInner(t, key, "u", UserMessage{Type: "user_message", Content: "hi"}))
	r.Wait()
	msgs := cap.decodeAll(t, key)
	last := msgs[len(msgs)-1]
	if !last.encrypted || last.msg["code"] != CodeOllamaUnavailable {
		t.Fatalf("want OLLAMA_UNAVAILABLE on stream start error, got %+v", last)
	}
}

func TestUserMessageMidStreamError(t *testing.T) {
	key := goldenKey(t)
	compute := &fakeCompute{enabled: true, stream: func(ctx context.Context) <-chan ollama.GenerateResult {
		out := make(chan ollama.GenerateResult, 2)
		out <- ollama.GenerateResult{Delta: ollama.GenerateDelta{Response: "tok"}}
		out <- ollama.GenerateResult{Err: fmt.Errorf("connection reset")}
		close(out)
		return out
	}}
	r, s, cap := testRouter(compute, key)
	r.Handle(context.Background(), s, encryptInner(t, key, "u", UserMessage{Type: "user_message", Content: "hi"}))
	r.Wait()
	msgs := cap.decodeAll(t, key)
	last := msgs[len(msgs)-1]
	// Mid-stream failure sends an encrypted error and no completed frame.
	if last.msg["type"] != "error" {
		t.Fatalf("want error last, got %v", last.msg)
	}
	for _, m := range msgs {
		if m.msg["type"] == "completed" {
			t.Fatal("completed must not follow a mid-stream error")
		}
	}
}
