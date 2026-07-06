# Contributing to silo-node

Thanks for helping build the Silo node. This document covers the codebase
layout, the rules that keep it safe, and how to get a change merged.

## Getting started

```bash
git clone https://github.com/onesilo/silo-node
cd silo-node
make build && make test && make lint
```

Go 1.24 or newer. No external services are needed for the test suite —
everything runs against `httptest` fakes.

## Layout

```
cmd/silo-node/        entrypoint + `healthcheck` subcommand
internal/config/      typed config, precedence loader (flags > env > TOML > defaults)
internal/logging/     slog setup
internal/node/        lifecycle reconciler + the Capability interface
internal/adminapi/    localhost admin API (127.0.0.1 only, token-authenticated)
internal/compute/     compute capability (Ollama-backed inference)
internal/tunnel/      Cloudflare quick-tunnel manager
internal/controlplane/ registration, heartbeats, token sources
```

### Architecture rules

- **One reconciler.** `internal/node` owns all lifecycle: it starts/stops
  capabilities, the tunnel, and registration to match live config. Nothing
  else spawns capability goroutines.
- **Capabilities are self-contained.** A capability implements the
  `Capability` interface (`Name`, `Enabled`, `Healthy`, `Start`, `Stop`) in
  its own package and must not import `internal/node` — the interface is
  satisfied structurally.
- **`internal/memory` must never import the pairing-key or LAN-serving
  packages.** Memory answers recall/remember requests; *who is allowed to
  ask* is decided by the control plane, and *how bytes stay confidential on
  the LAN* is the transport's job. Keeping memory ignorant of both makes it
  impossible to accidentally turn a transport secret into an authorization
  check. (The memory and lanserve packages land in a later phase; the rule
  applies from their first commit.)
- **Dependencies are a budget, not a convenience.** Currently:
  `BurntSushi/toml`, `google/uuid`, stdlib. `coder/websocket` and a zeroconf
  library arrive with the LAN phase. Anything else needs a strong case in
  the PR description.
- **No panics on start paths; goroutines exit on context cancellation.**
  Every background loop takes a `context.Context` and must return promptly
  when it is cancelled. Graceful shutdown deregisters from the control
  plane before exiting.
- **Fail closed.** Missing admin token → admin API refuses requests.
  Missing control-plane token → registration waits; it never sends
  unauthenticated requests.

## Making changes

1. Branch from `main`.
2. `make lint && make test` must pass (`gofmt`, `go vet`, `go test ./...`).
   New behavior needs tests; prefer `httptest` fakes over live services.
3. Keep commits focused and message subjects imperative
   ("Add heartbeat backoff", not "Added…").
4. Open a PR describing *why*, not just *what*. Contract changes against the
   control plane must link the backend change they track.

## Reporting security issues

Do not open public issues for vulnerabilities — email security@onesilo.com.
