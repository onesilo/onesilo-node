# silo-node

An open-source node for the [One Silo](https://onesilo.com) network. A node is a
machine you own — a Mac in your office, a home server, a container on your
NAS — that serves memory and LLM traffic from your own hardware.

## Modes

A node runs in one of two modes (`mode` in the config, chosen by the setup
wizard):

- **`local`** *(default)* — self-contained and private. Memory and LLM
  inference are served entirely from this machine; the node **never talks
  to the One Silo control plane**. Tunnels are rejected by config
  validation in this mode — there is nothing to phone home to.
- **`gateway`** — a relay to the One Silo control plane. The node connects
  with its own credentials (an `sc_` API key or a desktop-pushed JWT) and
  exposes the cloud surface — **cloud silos, connectors, and the MCP
  gateway** — to local clients at `/v1/cloud/*` on `lan.port`,
  authenticated by the node key. Local clients get the full One Silo
  platform without ever holding cloud credentials themselves. Gateway mode
  can *also* run the local capabilities, and it is the mode where tunnels
  and destination registration (cloud → node traffic) live.

## Capabilities

A node contributes independently enable-able capabilities:

| Capability | Status | What it provides |
|------------|--------|------------------|
| **compute** | available | Local LLM inference backed by [Ollama](https://ollama.com) (`llm_inference`) |
| **memory** | available | Silos homed on your device (`silo_recall`, `silo_remember`) |
| **lan** | available | LAN serving: the Silo iOS app chats with this node directly over the local network (Bonjour discovery + E2E-encrypted WebSocket, see [docs/protocol.md](docs/protocol.md)); also hosts the memory API and the gateway relay |
| **gateway** | available | Control-plane relay (`mode = "gateway"`): cloud silos, connectors, and MCP served to local clients at `/v1/cloud/*` |

Enable any subset. While at least one capability is enabled and the node has
a public URL (see tunnels below), it registers itself with the Silo control
plane as a *destination*, heartbeats every ~30 seconds with per-capability
liveness, and deregisters on shutdown.

silo-node ships two ways:

- **Bundled inside Silo Desktop** — the Mac app runs the binary and drives
  it over the localhost admin API (config, auth tokens, lifecycle). You
  never interact with it directly.
- **Standalone / Docker** — run it yourself with a TOML config file and an
  `sc_` API key. A reproducible distroless image and a compose file with an
  Ollama sidecar ship in this repo — see
  [docs/deploy-docker.md](docs/deploy-docker.md).

## Quickstart

```bash
make build              # Go 1.24+
./bin/silo-node setup   # interactive wizard (or `setup -yes` for defaults)

export SILO_API_KEY="sc_..."   # gateway mode only — from your Silo account
./bin/silo-node
```

`setup` asks for what it needs and provisions the rest:

- asks which **mode** the node runs in — `local` (private, the default) or
  `gateway` (One Silo relay) — and branches the remaining steps on it;
- generates the admin API token at `~/.silo-node/admin.token` (0600) —
  loaded automatically at start, no env var needed
  (`SILO_NODE_ADMIN_TOKEN` still wins when set);
- finds a running Ollama server or an existing install; when there is
  neither, it downloads the official Ollama release into the data dir and
  pulls the default model, so compute works with nothing pre-installed;
- optionally enables device memory (pulling the embedding model for hybrid
  recall);
- gateway mode only: optionally turns on the Cloudflare quick tunnel,
  downloading cloudflared the same way, and switches auth to an `sc_` API
  key;
- writes it all to `~/.silo-node/config.toml`.

Re-running `setup` is safe — it keeps previous choices as defaults. Check
the running node:

```bash
curl -s -H "Authorization: Bearer $(cat ~/.silo-node/admin.token)" \
  http://127.0.0.1:8766/v1/status | jq
```

Prefer hand-written config? Copy
[`config.example.toml`](config.example.toml) to `~/.silo-node/config.toml`
— it documents every option — and every knob is also a CLI flag / env var
(`silo-node -h`).

## Configuration

Precedence: **CLI flags > `SILO_NODE_*` env vars > TOML file > defaults**.
`silo-node -h` lists every flag with its matching env var.

| Section | Key | Default | Notes |
|---------|-----|---------|-------|
| — | `mode` | `local` | `local` (self-contained, no cloud) or `gateway` (control-plane relay) |
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
requires `Authorization: Bearer <admin token>`. The token comes from
`SILO_NODE_ADMIN_TOKEN` when set, else from `<data_dir>/admin.token`
(written by `silo-node setup`); with neither present the API fails closed.

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
NODE_KEY=$(curl -s -H "Authorization: Bearer $(cat ~/.silo-node/admin.token)" \
  http://127.0.0.1:8766/v1/status | jq -r .node_key)
curl -s -X POST -H "X-Silo-Node-Key: $NODE_KEY" \
  -d '{"content": "the deploy runs at 9am"}' \
  http://127.0.0.1:8765/v1/memory/personal/remember
```

Remember writes embeddings best-effort: if compute is off (or the embed
model isn't pulled), the memory is still stored and findable by keyword.

## Gateway relay API

With `mode = "gateway"`, the node relays the One Silo cloud surface on
**`lan.port`** under `/v1/cloud/`, attaching its own control-plane
credentials to every forwarded request. Local clients authenticate with the
same `X-Silo-Node-Key` header as the memory API and never see a cloud
token.

| Local route | Forwards to |
|-------------|-------------|
| `/v1/cloud/api/...` | `<control_plane.url>/api/...` (REST: cloud silos, connectors, ingestion) |
| `/v1/cloud/mcp` | `<control_plane.url>/mcp` (the MCP gateway, SSE streaming included) |

Only those two surfaces are relayed; every other path is refused. Example —
recall from a cloud silo through the node:

```bash
curl -s -X POST -H "X-Silo-Node-Key: $NODE_KEY" \
  -d '{"query": "payments migration"}' \
  http://127.0.0.1:8765/v1/cloud/api/v1/silos/default/recall
```

A local-mode node serves none of this: the `/v1/cloud/*` routes answer 503
and the node never opens a connection to the control plane.

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
