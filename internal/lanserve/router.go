package lanserve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/onesilo/silo-node/internal/compute/ollama"
)

// ComputeBackend is the slice of the compute capability the LAN router
// needs (satisfied by *compute.Capability; faked in tests).
type ComputeBackend interface {
	Enabled() bool
	StreamGenerate(ctx context.Context, prompt string, temperature float64) (<-chan ollama.GenerateResult, error)
}

// Session is one connected WebSocket client. Writes are serialized; the
// in-flight generation (if any) is tracked so interrupt/stop can cancel it.
type Session struct {
	writeMu sync.Mutex
	write   func(ctx context.Context, data []byte) error

	genMu     sync.Mutex
	genCancel context.CancelFunc
}

// NewSession wraps a frame-write function (one call per WebSocket text
// frame) in a Session.
func NewSession(write func(ctx context.Context, data []byte) error) *Session {
	return &Session{write: write}
}

func (s *Session) sendJSON(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.write(ctx, data)
}

// sendEncrypted seals an inner message and wraps it in the Envelope,
// echoing the client's user_id (SiloMac's ServingMessageSender.sendEncrypted).
func (s *Session) sendEncrypted(ctx context.Context, key []byte, userID string, v any) error {
	inner, err := json.Marshal(v)
	if err != nil {
		return err
	}
	combined, err := Seal(key, inner)
	if err != nil {
		return err
	}
	return s.sendJSON(ctx, Envelope{
		Content: base64.StdEncoding.EncodeToString(combined),
		UserID:  userID,
	})
}

// setGeneration records the cancel func of a newly started generation and
// returns it. Like SiloMac, a new user_message replaces the tracked task
// without cancelling the previous one.
func (s *Session) setGeneration(cancel context.CancelFunc) {
	s.genMu.Lock()
	s.genCancel = cancel
	s.genMu.Unlock()
}

// CancelGeneration cancels the in-flight generation, if any.
func (s *Session) CancelGeneration() {
	s.genMu.Lock()
	cancel := s.genCancel
	s.genCancel = nil
	s.genMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Router dispatches inbound frames: plain health checks and error paths
// stay unencrypted; everything else is decrypted with the pairing key and
// routed by inner "type". Port of SiloMac's MessageRouter.
type Router struct {
	key     func() []byte // current pairing key, nil when unpaired
	compute ComputeBackend
	logger  *slog.Logger

	// wg tracks generation goroutines so tests and shutdown can wait.
	wg sync.WaitGroup
}

// NewRouter builds a router. key is resolved per message so a pairing key
// stored mid-connection applies immediately. compute may be nil (memory-only
// node); user_message then gets an encrypted OLLAMA_UNAVAILABLE error.
func NewRouter(key func() []byte, compute ComputeBackend, logger *slog.Logger) *Router {
	return &Router{key: key, compute: compute, logger: logger}
}

// Wait blocks until all in-flight generations have finished.
func (r *Router) Wait() { r.wg.Wait() }

// Handle routes one inbound frame. Generation for user_message runs in a
// tracked goroutine so the caller's read loop keeps servicing interrupt and
// stop frames; everything else is handled synchronously.
func (r *Router) Handle(ctx context.Context, s *Session, frame []byte) {
	// Envelope iff both "content" and "user_id" decode as strings — the
	// same condition under which Swift's Codable decode of
	// EncryptedWebSocketMessage succeeds.
	var probe struct {
		Content *string `json:"content"`
		UserID  *string `json:"user_id"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil || probe.Content == nil || probe.UserID == nil {
		r.handlePlain(ctx, s, frame)
		return
	}

	key := r.key()
	if key == nil {
		s.sendJSON(ctx, newError(
			"Pairing required: enter this iPhone's pairing key in Silo → Settings → Sharing on this device",
			CodeNoKey))
		return
	}

	combined, err := base64.StdEncoding.DecodeString(*probe.Content)
	if err != nil {
		s.sendJSON(ctx, newError("Invalid base64 content", CodeInvalidBase64))
		return
	}
	plaintext, err := Open(key, combined)
	if err != nil {
		s.sendJSON(ctx, newError(
			"Failed to decrypt message — the pairing key on this device may not match this iPhone",
			CodeDecryptionFailed))
		return
	}

	r.routeInner(ctx, s, key, *probe.UserID, plaintext)
}

// handlePlain answers a plain {"type":"health_check"} probe (pre-pairing
// connectivity check); anything else plain is INVALID_FORMAT.
func (r *Router) handlePlain(ctx context.Context, s *Session, frame []byte) {
	var msg map[string]any
	if err := json.Unmarshal(frame, &msg); err == nil {
		if t, _ := msg["type"].(string); t == "health_check" {
			content, _ := msg["content"].(string)
			s.sendJSON(ctx, HealthCheckResponse{Type: "health_check_response", Content: "pong: " + content})
			return
		}
	}
	s.sendJSON(ctx, newError("Invalid message format - expected EncryptedWebSocketMessage", CodeInvalidFormat))
}

func (r *Router) routeInner(ctx context.Context, s *Session, key []byte, userID string, plaintext []byte) {
	var head struct {
		Type *string `json:"type"`
	}
	if err := json.Unmarshal(plaintext, &head); err != nil || head.Type == nil {
		// Plain error, no code — matches SiloMac.
		s.sendJSON(ctx, newError("Missing message type in decrypted message", ""))
		return
	}

	switch *head.Type {
	case "health_check":
		var req HealthCheckRequest
		if err := json.Unmarshal(plaintext, &req); err != nil {
			s.sendJSON(ctx, newError("Failed to route message: "+err.Error(), ""))
			return
		}
		s.sendEncrypted(ctx, key, userID, HealthCheckResponse{
			Type: "health_check_response", Content: "pong: " + req.Content,
		})

	case "user_message":
		var msg UserMessage
		if err := json.Unmarshal(plaintext, &msg); err != nil {
			s.sendJSON(ctx, newError("Failed to route message: "+err.Error(), ""))
			return
		}
		genCtx, cancel := context.WithCancel(ctx)
		s.setGeneration(cancel)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer cancel()
			r.generate(ctx, genCtx, s, key, userID, msg)
		}()

	case "interrupt", "stop":
		r.logger.Debug("generation cancelled by client", "type", *head.Type)
		s.CancelGeneration()

	default:
		// Plain error, no code — matches SiloMac.
		s.sendJSON(ctx, newError("Unknown message type: "+*head.Type, ""))
	}
}

// generate streams a completion for one user_message. connCtx outlives the
// generation (post-interrupt metadata/completed frames still go out, as in
// SiloMac); genCtx is cancelled by interrupt/stop.
func (r *Router) generate(connCtx, genCtx context.Context, s *Session, key []byte, userID string, msg UserMessage) {
	enc := func(v any) error { return s.sendEncrypted(connCtx, key, userID, v) }

	if err := enc(newStatus("processing", "Processing your request...")); err != nil {
		return
	}

	if r.compute == nil || !r.compute.Enabled() {
		enc(newError(
			"Local inference is not available on this node — enable the compute capability (Ollama) to chat over the LAN.",
			CodeOllamaUnavailable))
		return
	}

	temperature := 0.7
	if msg.Temperature != nil {
		temperature = *msg.Temperature
	}

	if err := enc(newStatus("generating", "Generating response...")); err != nil {
		return
	}

	start := time.Now()
	stream, err := r.compute.StreamGenerate(genCtx, msg.Content, temperature)
	if err != nil {
		enc(newError("Failed to generate response: "+err.Error(), CodeOllamaUnavailable))
		return
	}

	index := 0
	inputTokens := 0
	var firstToken time.Time
	for res := range stream {
		if res.Err != nil {
			enc(ErrorMessage{Type: "error", Error: "Failed to generate response: " + res.Err.Error(), Recoverable: true})
			return
		}
		if res.Delta.PromptEvalCount > 0 {
			inputTokens = res.Delta.PromptEvalCount
		}
		if res.Delta.Response != "" {
			if firstToken.IsZero() {
				firstToken = time.Now()
			}
			if err := enc(ContentDelta{Type: "content_delta", Delta: res.Delta.Response, Index: index}); err != nil {
				return
			}
			index++
		}
		if res.Delta.Done {
			break
		}
	}

	// Interrupted streams still get metadata + completed, like SiloMac.
	total := time.Since(start).Seconds()
	thinking := 0.0
	if !firstToken.IsZero() {
		thinking = firstToken.Sub(start).Seconds()
	}
	generation := total - thinking
	tokensPerSecond := 0.0
	if generation > 0 {
		tokensPerSecond = float64(index) / generation
	}
	enc(LLMResponseMetadata{
		Type:                   "llm_response_metadata",
		ThinkingTimeSeconds:    thinking,
		AverageTokensPerSecond: tokensPerSecond,
		TotalInputTokens:       inputTokens,
		TotalOutputTokens:      index,
	})
	enc(CompletedMessage{Type: "completed", FinishReason: "stop"})
}
