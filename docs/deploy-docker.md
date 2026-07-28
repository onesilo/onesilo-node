# Deploying onesilo-node with Docker

The repo ships a reproducible, distroless Docker image and a
`docker-compose.yml` that pairs the node with an [Ollama](https://ollama.com)
sidecar for the compute capability. All configuration is environment-driven —
every `SILO_NODE_*` variable maps 1:1 to a config key (`onesilo-node -h` lists
them; the table lives in `internal/config/load.go`).

## Quickstart

```bash
export SILO_API_KEY=sc_...          # see "Attach to your Silo account" below
docker compose up -d
docker compose ps                    # wait for onesilo-node to report (healthy)
docker compose logs -f onesilo-node
```

On first start the node generates its identity under the `silo-data` volume,
waits for Ollama to become healthy, opens a Cloudflare quick tunnel, and
registers with the control plane as a destination for your account.

Pull an Ollama model once (models persist in the `ollama-models` volume):

```bash
docker compose exec ollama ollama pull llama3.2:3b      # default chat model
docker compose exec ollama ollama pull nomic-embed-text # embeddings for hybrid memory recall
```

## Attach to your Silo account

Headless nodes authenticate with an `sc_` API key tied to a **Silo Connect
connection** of app type `silo_node`:

1. Create a connection with `app_type: silo_node` — in the Silo app under
   **Connect → Connections → New**, or directly against the API:

   ```bash
   curl -X POST https://api.onesilo.com/api/v1/connect/connections \
     -H "Authorization: Bearer <your JWT>" \
     -H "Content-Type: application/json" \
     -d '{"name": "home-server", "app_type": "silo_node"}'
   ```

2. The response contains the `sc_...` API key. **It is shown exactly once** —
   store it somewhere safe (rotating it later issues a new key and invalidates
   the old one).

3. Pass it to the container as `SILO_API_KEY` and set
   `SILO_NODE_AUTH_MODE=api_key` (the compose file already does the latter).

The key is identity only; revoking the connection immediately detaches the
node.

## Capabilities

Enable any subset; the node registers while at least one capability is on and
a public URL is available:

| Env var | Default | Effect |
|---------|---------|--------|
| `SILO_NODE_MEMORY` | `false` | Silos homed on this node (`silo_recall` / `silo_remember`). Stores are SQLite files under `/data`. |
| `SILO_NODE_COMPUTE` | `false` | Local LLM inference through Ollama (`SILO_NODE_OLLAMA_HOST`, `http://ollama:11434` in compose). |
| `SILO_NODE_LAN_ENABLED` | `false` | LAN serving for the Silo iOS app. Bonjour discovery does not cross the Docker network boundary, so LAN mode in a container requires publishing port 8765 (uncomment `ports` in compose) and connecting by IP. |

Memory-only nodes work without the Ollama sidecar (recall falls back to
keyword search); with compute enabled, `SILO_NODE_MEMORY_EMBED_MODEL`
(default `nomic-embed-text`) upgrades recall to hybrid vector + FTS.

## Tunnels

The node needs a public HTTPS URL to be reachable by the control plane.

**Quick tunnel (default in compose)** — `SILO_NODE_TUNNEL_MODE=quick` spawns
the bundled `/cloudflared` binary and gets an ephemeral
`*.trycloudflare.com` URL. Zero setup, but the hostname changes on every
restart (the node re-registers automatically).

**Named tunnel (stable hostname)** — run cloudflared yourself and tell the
node its public URL:

1. Create a named tunnel: `cloudflared tunnel create onesilo-node`, route it to
   the container (`ingress` → `http://onesilo-node:8765`), and note the tunnel
   UUID.
2. Run `cloudflare/cloudflared` as another compose service with
   `tunnel run --token ...`, attached to the same network.
3. Point the node at the tunnel's stable hostname:

   ```yaml
   environment:
     SILO_NODE_TUNNEL_MODE: external
     SILO_NODE_TUNNEL_EXTERNAL_URL: https://<tunnel-uuid>.cfargotunnel.com
   ```

   Any HTTPS URL that reaches the node's LAN port (8765) works, including a
   custom domain routed through the tunnel.

In `external` mode the node never spawns cloudflared; it just advertises the
URL to the control plane.

## Admin API stays loopback-only

The admin API binds `127.0.0.1:8766` **inside the container**
(`internal/adminapi/server.go`) and cannot be published with `ports:` — this
is deliberate. Docker's `HEALTHCHECK` runs `/onesilo-node healthcheck` inside
the container network namespace, so health status still works.

The image is distroless (no shell, no curl), so you cannot `docker exec` into
it. Operate the node through environment variables: change the env, then
`docker compose up -d` to recreate. If you need the authenticated admin API
(e.g. `GET /v1/status`), set `SILO_NODE_ADMIN_TOKEN` and query it from a
debug sidecar sharing the network namespace:

```bash
docker run --rm --network container:<onesilo-node-container> curlimages/curl \
  -H "Authorization: Bearer $SILO_NODE_ADMIN_TOKEN" \
  http://127.0.0.1:8766/v1/status
```

## Data persistence

`/data` (the `silo-data` named volume) holds everything the node must keep:

- `device_id` — stable identity with the control plane; losing it registers
  the node as a brand-new destination,
- `pairing.key` — E2E key for LAN serving,
- memory silo SQLite databases,
- `config.toml` — written only if config is changed via the admin API
  (`SILO_NODE_CONFIG=/data/config.toml`); env vars still override it.

Back up the volume to preserve node identity and memories. Ollama models
live in the separate `ollama-models` volume and are re-pullable.

## Upgrades

```bash
git pull
docker compose build onesilo-node
docker compose up -d
```

- State in `/data` is preserved; the node keeps its device identity.
- The node deregisters on SIGTERM and re-registers on start; expect a few
  seconds of unavailability. Quick-tunnel deployments come back on a new
  hostname automatically.
- Base images, cloudflared, and Ollama are digest-pinned in `Dockerfile` /
  `docker-compose.yml`; bumping them is an explicit, reviewable change.

## Reproducible builds

The image is built for byte-for-byte reproducibility:

- build and runtime base images pinned by digest,
- `CGO_ENABLED=0 GOFLAGS=-trimpath` and `-ldflags "-s -w -buildid="` make
  the Go binary deterministic,
- cloudflared is fetched at a pinned version and verified against a pinned
  SHA-256 (amd64 + arm64),
- `scripts/build-image.sh` sets `SOURCE_DATE_EPOCH` to the commit timestamp
  and builds with BuildKit's `--output type=docker,rewrite-timestamp=true`,
  which normalizes all layer file timestamps (requires buildx/BuildKit ≥
  0.13 — the script fails loudly otherwise).

Verify a build yourself:

```bash
make image-verify      # = scripts/build-image.sh
```

This builds the image twice — the second time with `--no-cache` — and fails
unless both the embedded `/onesilo-node` binary SHA-256 **and** the full image
IDs are identical. To audit a published image, run the script at the release
tag and compare the printed image ID and binary SHA-256 against the release
notes.

`make image` (single build with the same flags) prints the image ID and
binary hash without the double-build check.
