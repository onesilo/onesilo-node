# silo-node

An open-source node for the [Silo](https://onesilo.com) network. A node is a
machine you own — a Mac in your office, a home server, a container on your
NAS — that contributes capabilities to your Silo account and stays reachable
through a Cloudflare tunnel, so your Silo apps can use *your* hardware from
anywhere.

## Capabilities

A node contributes independently enable-able capabilities:

| Capability | Status | What it provides |
|------------|--------|------------------|
| **compute** | available | Local LLM inference backed by [Ollama](https://ollama.com) (`llm_inference`) |
| **memory** | available | Silos homed on your device (`silo_recall`, `silo_remember`) |
| **lan** | available | LAN serving: the Silo iOS app chats with this node directly over the local network (Bonjour discovery + E2E-encrypted WebSocket, see [docs/protocol.md](docs/protocol.md)); also hosts the memory API |

Enable any subset. While at least one capability is enabled and the node has
a public URL (see tunnels below), it registers itself with the Silo control
plane as a *destination*, heartbeats every ~30 seconds with per-capability
liveness, and deregisters on shutdown.

silo-node ships two ways:

- **Bundled inside Silo Desktop** — the Mac app runs the binary and drives
  it over the localhost admin API (config, auth tokens, lifecycle). You
  never interact with it directly.
- **Standalone / Docker** — run it yourself with a TOML config file and an
  `sc_` API key.

## Quickstart

```bash
# Build (Go 1.24+)
make build

# First run: enable compute against a local Ollama install
export SILO_NODE_ADMIN_TOKEN="$(openssl rand -hex 32)"   # guards the admin API
export SILO_API_KEY="sc_..."                             # from your Silo account
./bin/silo-node \
  -compute=true \
  -auth-mode=api_key \
  -tunnel-mode=quick        # requires cloudflared installed
```

The node connects to (or, with `-ollama-manage=true`, spawns) Ollama, opens
an ephemeral Cloudflare quick tunnel, and registers with the control plane.
Check it:

```bash
curl -s -H "Authorization: Bearer $SILO_NODE_ADMIN_TOKEN" \
  http://127.0.0.1:8766/v1/status | jq
```

For persistent setups, copy [`config.example.toml`](config.example.toml) to
`~/.silo-node/config.toml` — it documents every option.

## Configuration

Precedence: **CLI flags > `SILO_NODE_*` env vars > TOML file > defaults**.
`silo-node -h` lists every flag with its matching env var.

| Section | Key | Default | Notes |
|---------|-----|---------|-------|
| — | `data_dir` | `~/.silo-node` | device id, pairing key, persisted config |
| `log` | `format` / `level` | `text` / `info` | `json` for shippers |
| `capabilities` | `memory`, `compute` | `false` | independently toggled |
| `control_plane` | `url` | `https://api.onesilo.com` | |
| | `auth_mode` | `jwt` | `jwt` (desktop-pushed) or `api_key` (`SILO_API_KEY`) |
| | `device_name` | hostname | shown in Silo apps |
| `memory` | `embed_model` | `nomic-embed-text` | Ollama embedding model for hybrid recall |
| `ollama` | `host` | `http://127.0.0.1:11434` | |
| | `manage` | `false` | spawn `ollama serve` when unreachable |
| | `default_model` | `llama3.2:3b` | falls back to first installed model |
| `tunnel` | `mode` | `off` | `off` / `quick` / `external` |
| | `external_url` | — | required for `external` |
| `lan` | `enabled`, `port` | `false`, `8765` | LAN serving (Bonjour + WebSocket); the server also starts when `capabilities.memory` is on, because the memory API rides the same port |
| `admin` | `port` | `8766` | localhost-only admin API |

Config changes made through the admin API (`PUT /v1/config`) are applied
live by the reconciler and persisted back to the config file. (A changed
`admin.port` takes effect on the next start.)

## Admin API

Bound to `127.0.0.1:<admin.port>` only. Every route except `GET /healthz`
requires `Authorization: Bearer $SILO_NODE_ADMIN_TOKEN`; if that variable is
unset the API fails closed.

| Route | Purpose |
|-------|---------|
| `GET /healthz` | liveness (no auth; used by `silo-node healthcheck` / Docker) |
| `GET /v1/status` | version, capability health, tunnel URL, registration state, LAN server status, memory status, and the node key |
| `GET /v1/config` | current configuration |
| `PUT /v1/config` | partial update; persisted + reconciled live |
| `POST /v1/auth/jwt` | push a fresh control-plane JWT (in-memory) |
| `POST /v1/auth/pairing-key` | store the LAN pairing key (64 hex chars) |
| `POST /v1/shutdown` | graceful shutdown (deregisters first) |

`silo-node healthcheck` probes `/healthz` and exits 0/1 — wire it to a
Docker `HEALTHCHECK`.

## Memory API

With `capabilities.memory = true`, the node stores silo memories in SQLite
(`<data_dir>/memory.db`) with FTS5 keyword search, fused with vector search
over Ollama embeddings whenever the compute capability is also enabled
(hybrid recall; keyword-only otherwise). The API is served on **`lan.port`**
(the same server as LAN LLM serving — it starts whenever `lan.enabled` or
`capabilities.memory` is true) and every route requires the
`X-Silo-Node-Key` header. The key is auto-generated on first start at
`<data_dir>/node.key` (0600) and readable via admin `GET /v1/status`
(`node_key`).

| Route | Body → Response |
|-------|------------------|
| `POST /v1/memory/{silo_id}/remember` | `{"content": "...", "metadata": {...}?}` → `{"id": "..."}` |
| `POST /v1/memory/{silo_id}/recall` | `{"query": "...", "limit": 10?}` → `{"results": [{"id", "content", "score", "metadata"}]}` |
| `GET /v1/memory/silos` | → `[{"silo_id": "...", "count": n}]` |
| `DELETE /v1/memory/{silo_id}/{memory_id}` | → `{"deleted": true}` |

```bash
NODE_KEY=$(curl -s -H "Authorization: Bearer $SILO_NODE_ADMIN_TOKEN" \
  http://127.0.0.1:8766/v1/status | jq -r .node_key)
curl -s -X POST -H "X-Silo-Node-Key: $NODE_KEY" \
  -d '{"content": "the deploy runs at 9am"}' \
  http://127.0.0.1:8765/v1/memory/personal/remember
```

Remember writes embeddings best-effort: if compute is off (or the embed
model isn't pulled), the memory is still stored and findable by keyword.

## Security posture

Three distinct mechanisms, three distinct jobs:

- **Connection auth (JWT / API key)** — authenticates this node *to the
  control plane*. Either a short-lived Clerk JWT pushed by the desktop app,
  or an `sc_` API key. This is identity, nothing more.
- **Device pairing key** — confidentiality layer for direct LAN
  connections between your own devices (AES-256-GCM on every WebSocket
  payload). It never leaves your machines and is not an authorization
  mechanism. `internal/memory` is forbidden from touching it.
- **Node key** (`data_dir/node.key`) — bearer credential for the memory
  HTTP API, distinct from the pairing key by design.
- **Per-silo authorization** — which silos a caller may recall from or
  remember into is **control-plane policy**, decided server-side per
  request. The node never makes authorization decisions from the pairing
  key or tunnel reachability.

Also: the admin API binds loopback only and fails closed without its token;
the pairing key and device id are written `0600` under `data_dir`.

## Development

```bash
make build      # bin/silo-node with version/commit ldflags
make test       # go test ./...
make lint       # go vet + gofmt check
```

See [CONTRIBUTING.md](CONTRIBUTING.md). Dependencies are deliberately
minimal: `BurntSushi/toml`, `google/uuid`, `coder/websocket`,
`libp2p/zeroconf` (Bonjour), `modernc.org/sqlite` (CGO-free), and the
standard library.

## License

Apache-2.0 — see [LICENSE](LICENSE).
