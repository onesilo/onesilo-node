package lanserve

// Wire messages. These mirror Silo/Models/LLMWebSocketModels.swift exactly:
// snake_case keys, optional fields omitted when empty (Swift's synthesized
// Codable uses encodeIfPresent for optionals). Only the subset the LAN
// serving path needs is defined here.

// --- inbound (decrypted inner payloads) ---

// HealthCheckRequest is the inner {"type":"health_check"} payload
// (WSHealthCheckRequest in Swift).
type HealthCheckRequest struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// UserMessage is the inner {"type":"user_message"} payload. iOS sends many
// more fields (recent_messages, model, attachments, …); the LAN path only
// consumes content and temperature, the rest is ignored — same as SiloMac's
// LLMMessageHandler.
type UserMessage struct {
	Type        string   `json:"type"`
	Content     string   `json:"content"`
	Temperature *float64 `json:"temperature,omitempty"`
}

// InterruptMessage / StopMessage both cancel the in-flight generation for
// the session.
type InterruptMessage struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}

// StopMessage halts generation ({"type":"stop"}).
type StopMessage struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}

// --- outbound ---

// StatusUpdate is {"type":"status"}: plain "connected" on accept, encrypted
// "processing"/"generating" during a turn.
type StatusUpdate struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// HealthCheckResponse is {"type":"health_check_response","content":"pong: X"}
// (WSHealthCheckResponse in Swift) — plain for pre-pairing probes, encrypted
// for paired ones.
type HealthCheckResponse struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// ContentDelta is one streamed token: {"type":"content_delta"}.
type ContentDelta struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	Index int    `json:"index"`
}

// LLMResponseMetadata carries generation performance metrics, sent after the
// last delta and before completed.
type LLMResponseMetadata struct {
	Type                   string  `json:"type"`
	ThinkingTimeSeconds    float64 `json:"thinking_time_seconds"`
	AverageTokensPerSecond float64 `json:"average_tokens_per_second"`
	TotalInputTokens       int     `json:"total_input_tokens"`
	TotalOutputTokens      int     `json:"total_output_tokens"`
}

// CompletedMessage ends a turn: {"type":"completed","finish_reason":"stop"}.
// usage is never sent on the LAN path (omitted, like Swift's nil optional).
type CompletedMessage struct {
	Type         string `json:"type"`
	FinishReason string `json:"finish_reason"`
}

// ErrorMessage is {"type":"error"}. code is optional; recoverable is always
// present (non-optional in Swift).
type ErrorMessage struct {
	Type        string `json:"type"`
	Error       string `json:"error"`
	Code        string `json:"code,omitempty"`
	Recoverable bool   `json:"recoverable"`
}

// Error codes for plain (unencrypted) error frames, matching SiloMac's
// MessageRouter exactly.
const (
	CodeInvalidFormat    = "INVALID_FORMAT"
	CodeNoKey            = "NO_KEY"
	CodeInvalidBase64    = "INVALID_BASE64"
	CodeDecryptionFailed = "DECRYPTION_FAILED"
	// CodeOllamaUnavailable is sent encrypted when a user_message arrives
	// but compute cannot serve it.
	CodeOllamaUnavailable = "OLLAMA_UNAVAILABLE"
	// CodePairingUnverified is sent encrypted when a user_message arrives on
	// an automated-pairing session whose first-contact SAS has not yet been
	// confirmed by the operator.
	CodePairingUnverified = "PAIRING_UNVERIFIED"
)

func newStatus(status, message string) StatusUpdate {
	return StatusUpdate{Type: "status", Status: status, Message: message}
}

func newError(msg, code string) ErrorMessage {
	return ErrorMessage{Type: "error", Error: msg, Code: code, Recoverable: true}
}
