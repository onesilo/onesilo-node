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
| **memory** | later release | Silos homed on your device (`silo_recall`, `silo_remember`) |

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
| `ollama` | `host` | `http://127.0.0.1:11434` | |
| | `manage` | `false` | spawn `ollama serve` when unreachable |
| | `default_model` | `llama3.2:3b` | falls back to first installed model |
| `tunnel` | `mode` | `off` | `off` / `quick` / `external` |
| | `external_url` | — | required for `external` |
| `lan` | `enabled`, `port` | `false`, `8765` | LAN serving arrives in a later release |
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
| `GET /v1/status` | version, capability health, tunnel URL, registration state |
| `GET /v1/config` | current configuration |
| `PUT /v1/config` | partial update; persisted + reconciled live |
| `POST /v1/auth/jwt` | push a fresh control-plane JWT (in-memory) |
| `POST /v1/auth/pairing-key` | store the LAN pairing key (64 hex chars) |
| `POST /v1/shutdown` | graceful shutdown (deregisters first) |

`silo-node healthcheck` probes `/healthz` and exits 0/1 — wire it to a
Docker `HEALTHCHECK`.

## Security posture

Three distinct mechanisms, three distinct jobs:

- **Connection auth (JWT / API key)** — authenticates this node *to the
  control plane*. Either a short-lived Clerk JWT pushed by the desktop app,
  or an `sc_` API key. This is identity, nothing more.
- **Device pairing key** — legacy confidentiality layer for direct LAN
  connections between your own devices. It never leaves your machines and
  is not an authorization mechanism.
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
minimal: `BurntSushi/toml`, `google/uuid`, and the standard library.

## License

Apache-2.0 — see [LICENSE](LICENSE).
