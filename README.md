# onesilo-node

An open-source node for the [One Silo](https://onesilo.com) network. A node is a
machine you own — a Mac in your office, a home server, a container on your
NAS — that serves memory and LLM traffic from your own hardware.

## Modes & remote access

A node has **two independent axes**, both switchable from the setup control
panel:

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

## Which way to run it

**Most people should use [Silo Desktop](https://onesilo.com).** The Mac app
bundles this binary and drives it over the localhost admin API — config,
auth tokens, lifecycle — so the node is installed, updated and supervised
for you and there is nothing to run in a terminal. That is the supported
path for anyone who just wants a node.

**This repository is for running it yourself**, and assumes you are
comfortable on a command line. Three ways — they produce the same node, so
pick by where it is running:

| Method | Best for | What it takes |
|---|---|---|
| **Homebrew** | a Mac or Linux workstation | `brew tap onesilo/tap`, `brew trust --formula onesilo/tap/onesilo-node`, `brew install onesilo-node` — builds from source, then `onesilo-node setup`. See below for why the trust step is required. |
| **Docker** | a NAS, a home server, anything long-running | A reproducible distroless image and a compose file with an Ollama sidecar — see [docs/deploy-docker.md](docs/deploy-docker.md). |
| **From source** | hacking on it, or a machine you already develop on | `make build` (see [Quickstart](#quickstart)). |
| **`go install`** | the quickest start if you already have Go | `go install github.com/onesilo/onesilo-node/cmd/onesilo-node@latest` |

`go.mod` requires Go 1.25, but you do not need it installed: `GOTOOLCHAIN`
defaults to `auto`, so any Go from 1.21 onward fetches the right toolchain
by itself. Both source paths work on whatever Go your distribution ships.

**There are no binary downloads, deliberately.** A tarball is the weakest
of the options above: on macOS it would be unsigned and Gatekeeper would
block it, and on Linux the image already does the job better — it carries a
checksum-pinned `cloudflared` with it, is addressed by digest rather than
filename, and is reproducible with its build provenance attested. Shipping
loose binaries as well would add the one artifact we could not stand behind
without also becoming an Apple-notarized distributor, which is what Silo
Desktop is for.

The Homebrew formula is not an exception to that. It **builds from source**
rather than fetching a prebuilt binary, so nothing is downloaded that
Gatekeeper would quarantine — you get the same binary `go install` produces,
with an upgrade path. Its flags match `scripts/verify-builds.sh` (`-trimpath`,
`-s -w -buildid=`, `CGO_ENABLED=0`) and it pins `GOTOOLCHAIN=local` so the
build cannot quietly fetch a different toolchain mid-install, rather than
becoming a third configuration nobody checks.

A brewed build is **not** byte-identical to a release artifact and does not
claim to be: the release build also injects `internal/version.Commit` from the
git SHA, which a source tarball does not carry. Verifying released binaries is
`scripts/verify-builds.sh`'s job.

The tap is live at [onesilo/homebrew-tap](https://github.com/onesilo/homebrew-tap):

```bash
brew tap onesilo/tap
brew trust --formula onesilo/tap/onesilo-node
brew install onesilo-node
```

The `trust` step is not optional on current Homebrew: it refuses to load
formulae from third-party taps until you trust them, so without it `install`
stops at *"Refusing to load formula … from untrusted tap"*. Trusting the
single formula is narrower than `brew trust onesilo/tap`, which also covers
anything the tap ships in future.

Because the formula compiles rather than pouring a bottle, Homebrew enforces
its floor for source builds and will refuse to continue on an outdated Xcode
or Command Line Tools. A current Xcode is often already installed but not
selected, so check that first:

```bash
xcode-select -p
sudo xcode-select --switch /Applications/Xcode.app/Contents/Developer
```

If it still reports outdated Command Line Tools:
`sudo rm -rf /Library/Developer/CommandLineTools && sudo xcode-select --install`.

Tags are still the stable versions — pin them with `go install …@v0.1.0` or
`ghcr.io/onesilo/onesilo-node:v0.1.0`.

## Quickstart

```bash
make build              # Go 1.25+
./bin/onesilo-node setup   # launches the node + control panel (or `setup -yes` for headless init)
```

`setup` launches the node with default settings — a **Local Node** with
Compute, Memory, and LAN discovery on — and drops you into a control panel:

```
Welcome to One Silo Node

Current Configuration:
  Mode:          Local
  Capabilities:  Compute, Memory, LAN

  1. Launch Admin Interface (127.0.0.1:8766)
  2. Switch to Local Relay
  3. Enable access from anywhere
  q. Quit (stops the node)
```

On first run it bootstraps what a working node can't do silently: it
generates the admin API token at `~/.onesilo-node/admin.token` (0600, loaded
automatically at start; `SILO_NODE_ADMIN_TOKEN` still wins when set), finds
a running Ollama server or an existing install — downloading the official
release into the data dir and pulling the default and embedding models when
there is neither — and writes `~/.onesilo-node/config.toml`.

The panel drives the running node live (changes persist to the config and
apply without a restart):

- **Launch Admin Interface** opens the browser signed in — models, silos,
  pairing approvals, and every setting live there;
- **Switch to Local Relay / Local Node** flips the mode axis. Becoming a
  relay signs you in to One Silo first — a browser OAuth flow, after which
  the node holds its own refreshable credential
  (`~/.onesilo-node/oauth.json`, 0600) and appears in your
  [dashboard connections](https://dashboard.onesilo.com/connections), just
  like the Silo iOS app;
- **Enable access from anywhere** flips the remote-access axis: it pairs
  with the control plane the same way when needed, downloads cloudflared if
  missing, and asks One Silo to provision a stable hostname for this node
  (requires a subscription — running the node locally is always free).

Re-running `setup` is safe and fast — an existing config is respected, and
`onesilo-node` without arguments still runs the node headless (Docker,
services, scripts; use an `sc_` API key via `SILO_API_KEY` there instead of
the browser sign-in). Check the running node:

```bash
curl -s -H "Authorization: Bearer $(cat ~/.onesilo-node/admin.token)" \
  http://127.0.0.1:8766/v1/status | jq
```

Prefer hand-written config? Copy
[`config.example.toml`](config.example.toml) to `~/.onesilo-node/config.toml`
— it documents every option — and every knob is also a CLI flag / env var
(`onesilo-node -h`).

## Configuration

Precedence: **CLI flags > `SILO_NODE_*` env vars > TOML file > defaults**.
`onesilo-node -h` lists every flag with its matching env var.

| Section | Key | Default | Notes |
|---------|-----|---------|-------|
| — | `mode` | `local` | `local` (self-contained, no cloud) or `gateway` (control-plane relay) |
| — | `data_dir` | `~/.onesilo-node` | device id, pairing key, persisted config |
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
(written by `onesilo-node setup`); with neither present the API fails closed.

| Route | Purpose |
|-------|---------|
| `GET /healthz` | liveness (no auth; used by `onesilo-node healthcheck` / Docker) |
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
| `GET /v1/logs/stream` | live log stream (Server-Sent Events): the retained recent records, then every new one as it happens |

`onesilo-node healthcheck` probes `/healthz` and exits 0/1 — wire it to a
Docker `HEALTHCHECK`.

### Running under a host app

A host app that spawns the node as a child process (Silo Desktop does) can
set `SILO_NODE_PARENT_PID` to its own pid. The node then polls that process
and shuts itself down gracefully when it goes away, instead of surviving a
host that was killed rather than quit — an orphan would keep holding
`admin.port` and the host's next launch would fail to bind it. Unset (the
Docker and headless case) means no parent supervision.

## Admin UI

The admin port also serves a dashboard at `http://127.0.0.1:8766/` —
embedded in the binary, no extra install. It asks for the admin token once
(kept in localStorage) and gives you four pages:

- **Silos** — every silo on the node with its memories: inspect, delete,
  and **Export .silo** (downloads the silo in the open
  [silo-spec](https://github.com/onesilo/silo-spec) format, readable by
  anything that speaks `.silo`).
- **Models** — the local LLM lineup: see what's installed, pull new models
  from the Ollama library with live progress, and activate the default
  model the node serves.
- **Logs** — a live console of what the node is doing right now, streamed
  as it happens, with a level filter, follow/pause, and the last 1000
  lines retained so opening the page mid-incident shows what led up to it.
  This is the only view of the node when it runs under `onesilo-node
  setup`'s control panel (which diverts logs to a file so they don't
  scribble over the screen) or under a supervisor that swallows stderr.
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
NODE_KEY=$(curl -s -H "Authorization: Bearer $(cat ~/.onesilo-node/admin.token)" \
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
make build      # bin/onesilo-node with version/commit ldflags
make test       # go test ./...
make lint       # go vet + gofmt check
```

See [CONTRIBUTING.md](CONTRIBUTING.md). Dependencies are deliberately
minimal: `BurntSushi/toml`, `google/uuid`, `coder/websocket`,
`libp2p/zeroconf` (Bonjour), `modernc.org/sqlite` (CGO-free), and the
standard library.

## Releases

Pushing a `v*` tag builds and publishes everything; there is no manual
step.

```bash
git tag v0.2.0 && git push origin v0.2.0
```

That pushes a multi-architecture image to
`ghcr.io/onesilo/onesilo-node` and creates a GitHub release whose notes
point at it. A tag with a hyphen (`v0.2.0-rc.1`) is marked as a prerelease
automatically.

**The image is the release.** No archives are published — see
[Which way to run it](#which-way-to-run-it) for why. A tag is a stable
version you can pin, whether you pull the image or
`go install …/cmd/onesilo-node@v0.2.0`.

**The tag is gated on two checks**, both of which publish nothing and exist
only to stop a bad release:

- **Every supported platform builds, reproducibly.** Each of
  linux/amd64, linux/arm64, darwin/amd64 and darwin/arm64 is compiled
  twice and the release fails if the two differ. darwin is included even
  though no macOS artifact ships, because Silo Desktop bundles this daemon
  for macOS — a change that breaks the Mac build should fail here, not
  there. Run it yourself with `make verify-builds`.
- **The binary reports the tag.** A missed ldflags path compiles cleanly
  and passes every test, so the release extracts the version and compares
  it to the tag exactly. This has caught real bugs twice.

The image gets the same treatment independently: `scripts/build-image.sh`
builds twice and fails if the image IDs diverge.

**Verify what you pulled.** The image carries build provenance attestation,
so you can confirm it came from this repository's release workflow at a
specific commit:

```bash
gh attestation verify oci://ghcr.io/onesilo/onesilo-node:v0.2.0 \
  --repo onesilo/onesilo-node
```

Run the workflow manually from the Actions tab for a dry run — it builds
and verifies everything without publishing.

## License

Apache-2.0 — see [LICENSE](LICENSE).
