# LAN serving protocol

The wire protocol the Silo iOS app speaks to a silo-node (or a SiloMac /
SiloDesktop install — the format is identical) on the local network. The Go
implementation lives in `internal/lanserve/` and is a port of the SiloMac
prototype (`SiloMac/Serving/*.swift`); the JSON shapes are defined by
`Silo/Models/LLMWebSocketModels.swift` on the client.

## Discovery (Bonjour)

The node advertises `_silo-llm._tcp.` on `local.` while `lan.enabled` is
true:

- **Instance name**: `<Hostname> Silo LLM`
- **Port**: `lan.port` (default 8765)
- **TXT record**:
  | key | value | notes |
  |---|---|---|
  | `model` | current Ollama model tag (e.g. `llama3.2:3b`) | the only key iOS parses; re-announced when the model changes |
  | `version` | `1.0` | legacy, matches SiloMac |
  | `capabilities` | `chat,stream,e2e` | legacy, matches SiloMac |
  | `protocol` | `1` | additive, silo-node only |

A node with only the memory capability enabled runs the same HTTP server
but does **not** publish Bonjour (there is no LLM to discover).

## Transport

Plain WebSocket (`ws://<host>:<port>/`, **root path**, no TLS — payloads
are end-to-end encrypted instead). All frames are JSON text frames. The
same port also serves:

- `GET /healthz` → `{"ok": true}` (plain HTTP liveness)
- `/v1/memory/*` → memory capability API (see README; authenticated with
  `X-Silo-Node-Key`, unrelated to the pairing key)

### Connection handshake

Immediately after the WebSocket is accepted, the server sends one **plain**
(unencrypted) frame:

```json
{"type": "status", "status": "connected", "message": "Connected to Silo LLM"}
```

The iOS client reads and skips this frame before anything else.

## Encryption envelope

Everything after pairing travels inside the envelope (both directions):

```json
{
  "content": "<base64 of AES-256-GCM combined data>",
  "user_id": "<opaque client id, unencrypted>"
}
```

- **Key**: the 32-byte pairing key, shared as 64 hex chars during the iOS
  pairing flow and pushed to the node via the admin API
  (`POST /v1/auth/pairing-key`); stored at `<data_dir>/pairing.key` (0600).
  The key is re-read per message, so pairing applies without a restart.
- **Combined format** (CryptoKit `AES.GCM.SealedBox.combined`):
  `nonce (12 bytes) || ciphertext || tag (16 bytes)`, so
  `len = 12 + len(plaintext) + 16`. In Go:
  `gcm.Seal(nonce, nonce, plaintext, nil)` with a fresh random 12-byte
  nonce; no additional authenticated data.
- `user_id` is never validated or decrypted; the server echoes the value
  from the request on every response envelope of that turn.

A frame is treated as an envelope iff both `content` and `user_id` decode
as strings; otherwise it takes the plain-message path below.

### Golden vector

| | |
|---|---|
| key (hex) | `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f` |
| nonce (hex) | `0102030405060708090a0b0c` |
| plaintext | `hello silo` |
| combined (base64) | `AQIDBAUGBwgJCgsMbY82uYO0g+8gzdqORRQ3Uq6oY97qKBlISOI=` |

Checked by `internal/lanserve/envelope_test.go`.

## Plain (unencrypted) messages

Only the pre-pairing health check is answered in plaintext:

```json
→ {"type": "health_check", "content": "abc"}
← {"type": "health_check_response", "content": "pong: abc"}
```

(A non-string `content` is treated as `""` → `"pong: "`.)

Any other plain frame gets a **plain** error:

```json
← {"type": "error", "error": "Invalid message format - expected EncryptedWebSocketMessage", "code": "INVALID_FORMAT", "recoverable": true}
```

### Plain error codes

| code | when |
|---|---|
| `INVALID_FORMAT` | frame is neither an envelope nor a plain health check |
| `NO_KEY` | envelope received but no pairing key is stored yet |
| `INVALID_BASE64` | envelope `content` is not valid base64 |
| `DECRYPTION_FAILED` | AES-GCM open failed (key mismatch / tampering) |

Two plain error cases carry **no** `code` (matching SiloMac): a decrypted
payload without a `"type"` field (`"Missing message type in decrypted
message"`) and an unknown inner type (`"Unknown message type: <t>"`).

## Encrypted inner messages

The envelope `content` decrypts to one of these JSON payloads.

### Client → server

| type | fields used by the node | action |
|---|---|---|
| `health_check` | `content` | encrypted `health_check_response` with `pong: <content>` (pairing verification) |
| `user_message` | `content`, `temperature` (default 0.7; other iOS fields are ignored) | streamed generation, see below |
| `interrupt` | `reason?` | cancel the in-flight generation |
| `stop` | `reason?` | same as `interrupt` |

### Server → client (all wrapped in the envelope)

**`user_message` turn**, in order:

1. `{"type":"status","status":"processing","message":"Processing your request..."}`
2. `{"type":"status","status":"generating","message":"Generating response..."}`
3. One frame per token:
   `{"type":"content_delta","delta":"<token>","index":0}` (index increments)
4. `{"type":"llm_response_metadata","thinking_time_seconds":0.42,"average_tokens_per_second":31.5,"total_input_tokens":7,"total_output_tokens":128}`
   - `thinking_time_seconds`: start → first token
   - `average_tokens_per_second`: output tokens / (total − thinking)
   - `total_input_tokens`: Ollama `prompt_eval_count`
   - `total_output_tokens`: number of deltas sent
5. `{"type":"completed","finish_reason":"stop"}` (`usage` is never sent)

**Interrupt/stop mid-stream**: deltas stop, but `llm_response_metadata`
and `completed` (`finish_reason: "stop"`) are **still sent** — this matches
SiloMac, and the iOS client relies on `completed` to end the turn.

**Errors during a turn** (encrypted `{"type":"error", ...,
"recoverable":true}`):

| code | when |
|---|---|
| `OLLAMA_UNAVAILABLE` | compute capability disabled, not running, or the stream could not start |
| *(none)* | generation failed mid-stream (`"Failed to generate response: …"`); no `completed` follows |

## Known divergences from the Swift prototype

Intentional, wire-compatible differences (codes and shapes match; only
lenience/text differ):

- Error **message texts** mentioning "this Mac" say "this device" instead;
  clients switch on `code`, not text.
- `user_message` decoding is lenient: SiloMac fails when the non-optional
  `use_openrouter` field is missing and replies with a generic routing
  error; silo-node ignores missing optional fields (iOS always sends it).
- SiloMac tracks a new generation per `user_message` without cancelling the
  previous one; silo-node behaves the same (the tracked cancel handle is
  replaced), and `interrupt`/`stop` cancel the most recent generation.
