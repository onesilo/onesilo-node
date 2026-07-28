# silo-node

An open-source node for the [One Silo](https://onesilo.com) network. A node is a
machine you own — a Mac in your office, a home server, a container on your
NAS — that serves memory and LLM traffic from your own hardware.

## Modes & remote access

A node has **two independent axes**, both chosen by the setup wizard:

**Mode** (`mode` in the config) — what the node *relays*:

- **`local`** *(default, "Local Node")* — memory and LLM inference served
  entirely from this machine. It does not relay the control plane's cloud
  surface.
- **`gateway`** *("Local Relay")* — additionally relays **cloud silos,
  connectors, and the MCP gateway** to local clients at `/v1/cloud/*` on
  `lan.port` (node-key authenticated), using its own control-plane
  credentials. Local clients get the full One Silo platform without ever
  holding cloud credentials themselves.

**Remote access** (`tunnel.mode`) — whether the node is *reachable* from the
control plane, independent of mode:

- **`off`** *(default)* — LAN/localhost only.
- **`managed`** / **`quick`** / **`external`** — the node runs a tunnel and
  **registers itself with the control plane as a destination**. Its local
  compute and memory become reachable from your One Silo–authenticated apps
  (the iOS app, web) anywhere. The LLM session is end-to-end encrypted to
  the node with the device pairing key; the control plane only provides
  discovery/routing.

  `managed` is the preferred option — One Silo provisions a named
  Cloudflare tunnel so the node keeps a **stable hostname** across
  restarts. `quick` spawns an ephemeral Cloudflare tunnel whose URL changes
  every restart (the node re-registers itself). `external` means you run
  your own ingress and set `external_url`.

  **Any mode other than `off` registers with the control plane, which
  requires a paid One Silo plan** — including `external`, where you supply
  the ingress yourself, because the node still registers as a destination.
  Without a plan the node logs the refusal, backs off, and keeps serving
  locally. Running a node locally is always free.

The two combine freely: a **Local Node with remote access on** serves purely
local compute/memory but is reachable from anywhere; a **Local Relay with
remote access off** relays the cloud to LAN clients but isn't itself exposed.
Any node that talks to the control plane — a relay, or an exposed node —
signs in during setup and must use an `https` control-plane URL.

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
make build              # Go 1.25+
./bin/silo-node setup   # interactive wizard (or `setup -yes` for defaults)

export SILO_API_KEY="sc_..."   # only if the node relays or is exposed — from your Silo account
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
- asks whether to **enable remote access** — a Cloudflare quick tunnel
  (downloading cloudflared the same way) that makes this node reachable from
  your authenticated apps anywhere. Available for a Local Node or a Local
  Relay;
- for a relay or an exposed node, **signs you in to One
  Silo** — a browser OAuth flow, after which the node holds its own
  refreshable credential (`~/.silo-node/oauth.json`, 0600) and appears in
  your [dashboard connections](https://dashboard.onesilo.com/connections),
  just like the Silo iOS app. No account yet? The wizard points you to
  [onesilo.com](https://onesilo.com) to create one. An `sc_` API key
  remains the headless fallback;
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
| | `auth_mode` | `jwt` | `jwt` (desktop-pushed), `api_key` (`SILO_API_KEY`), or `oauth` (setup sign-in) |
| | `device_name` | hostname | shown in Silo apps |
| `memory` | `embed_model` | `nomic-embed-text` | Ollama embedding model for hybrid recall |
| `ollama` | `host` | `http://127.0.0.1:11434` | |
| | `manage` | `false` | spawn `ollama serve` when unreachable |
| | `default_model` | `llama3.2:3b` | falls back to first installed model |
| `tunnel` | `mode` | `off` | `off` / `managed` / `quick` / `external`; anything but `off` registers with the control plane and needs a paid plan |
| | `external_url` | — | required for `external` |
| `lan` | `enabled`, `port` | `false`, `8765` | LAN serving (Bonjour + WebSocket); the server also starts when `capabilities.memory` is on, because the memory API rides the same port |
| | `require_pairing_verification` | `true` | withhold inference from a first-contact app identity key until its SAS is confirmed in the admin UI ([design](docs/automated-pairing.md)) |
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
| `POST /v1/compute/generate` | one-shot completion on the node's local model — lets local agents (e.g. a Buzz memory agent) distill privately before anything reaches the control plane; body `{"prompt": "...", "temperature": 0.2?}` |
| `POST /v1/shutdown` | graceful shutdown (deregisters first) |
| `GET /v1/silos` | silos with memory counts (backs the admin UI) |
| `GET /v1/silos/{silo_id}/memories` | every memory in a silo, unsealed |
| `DELETE /v1/silos/{silo_id}/memories/{memory_id}` | forget one memory |
| `GET /v1/silos/{silo_id}/export` | download the silo as a `.silo` package ([silo-spec](https://github.com/onesilo/silo-spec) v0.1.1) |
| `GET /v1/models` | installed Ollama models, active/default flags, pull progress |
| `POST /v1/models/pull` | start a background model pull; body `{"model": "..."}` |

`silo-node healthcheck` probes `/healthz` and exits 0/1 — wire it to a
Docker `HEALTHCHECK`.

## Admin UI

The admin port also serves a dashboard at `http://127.0.0.1:8766/` —
embedded in the binary, no extra install. It asks for the admin token once
(kept in localStorage) and gives you three pages:

- **Silos** — every silo on the node with its memories: inspect, delete,
  and **Export .silo** (downloads the silo in the open
  [silo-spec](https://github.com/onesilo/silo-spec) format, readable by
  anything that speaks `.silo`).
- **Models** — the local LLM lineup: see what's installed, pull new models
  from the Ollama library with live progress, and activate the default
  model the node serves.
- **Settings** — the full node configuration (local vs gateway mode,
  capabilities, control-plane URL + auth mode, connected OAuth account,
  Ollama, tunnel, LAN) with live status: registration, tunnel URL,
  capability health, and the node key.

The static page itself is served without auth — the admin server binds
loopback only — but every API call it makes carries the admin bearer
token.

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

- **Connection auth (OAuth / JWT / API key)** — authenticates this node
  *to the control plane*. Either the node's own OAuth grant from the setup
  sign-in (refresh token at `data_dir/oauth.json`, 0600, revocable from
  the dashboard), a short-lived Clerk JWT pushed by the desktop app, or an
  `sc_` API key. This is identity, nothing more.
- **Device pairing key** — confidentiality layer for direct LAN
  connections between your own devices (AES-256-GCM on every WebSocket
  payload). It never leaves your machines and is not an authorization
  mechanism. `internal/memory` is forbidden from touching it.
- **Pairing handshake** — the session key is established by an
  authenticated ECDH handshake rather than a shared secret typed in by
  hand. Static P-256 identity keys authenticate each side (P-256 rather
  than X25519 so the app's private key can live in the Secure Enclave),
  fresh ephemeral keys per connection give forward secrecy, and HKDF-SHA256
  binds the full handshake transcript into the derived key. The control
  plane vouches for device *public* keys and never holds the symmetric key,
  so it cannot decrypt a session it relays. Unknown app keys are pinned
  trust-on-first-use and — unless `lan.require_pairing_verification` is
  turned off — cannot run inference until their short authentication string
  is confirmed in the admin UI. Scanning a QR code into
  `POST /v1/auth/pairing-key` remains supported as a fallback. Design and
  threat model: [docs/automated-pairing.md](docs/automated-pairing.md).
- **Node key** (`data_dir/node.key`) — bearer credential for the memory
  HTTP API, distinct from the pairing key by design. It is **full memory
  access**: any holder can remember/recall/forget in any silo on the node,
  so treat it as a high-value secret. Comparisons are constant-time over
  SHA-256 digests (no length leak); the memory API is fail-closed.
- **Per-silo authorization** — which silos a caller may recall from or
  remember into is **control-plane policy**, decided server-side per
  request. The node never makes authorization decisions from the pairing
  key or tunnel reachability.

**Encryption at rest.** Memory content is sealed with **AES-256-GCM** under
a per-node key at `data_dir/memory.key` (0600), a fresh random nonce per
record, and the silo id bound in as additional authenticated data — so a
stored blob cannot be decrypted under, or moved to, a different silo, and
tampering fails the GCM tag. The FTS5 keyword index is contentless
(`content=''`), so no plaintext leaks through it. Reading `memory.db`
directly yields only ciphertext.

**Transport.** When the node talks to the control plane (a relay, or an
exposed node) the control-plane URL must be `https://` (loopback `http://`
is allowed for local dev), and OAuth discovery
endpoints are pinned to the issuer's origin and required to be https — so
authorization codes, PKCE verifiers, and refresh tokens never transit
plaintext or reach an attacker-chosen host. Credential files are written
atomically (temp + rename) so a crash can't truncate them.

**Admin API.** Binds loopback only, fails closed without its token, and
enforces a loopback `Host` allowlist (defense-in-depth against
DNS-rebinding). The embedded UI's static assets are unauthenticated (the
server is loopback-only) but every API call it makes carries the token.

**LAN server.** The memory/relay port is on `0.0.0.0` for on-network use,
so it caps request-body size, clamps recall `limit`, bounds concurrent
WebSocket connections, and applies an idle read deadline. The gateway relay
forwards only the node's own bearer token upstream — it strips
client-supplied `Cookie`/`X-Forwarded-*`/`Forwarded` headers and restricts
methods and paths.

The data directory itself is `0700` (repaired at startup if pre-existing);
`oauth.json`, `node.key`, `memory.key`, `admin.token`, `pairing.key`, and
the device id are all written `0600`.

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
