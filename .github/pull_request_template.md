<!--
Describe *why*, not just what. See CONTRIBUTING.md.
Security issue? Do not open a PR — email security@onesilo.com.
-->

## What and why

<!-- What changes, and what problem it solves. -->

## Checks

- [ ] `make lint && make test` pass
- [ ] New behavior has tests (`httptest` fakes preferred over live services)
- [ ] No new dependency, or the PR explains why one is warranted
- [ ] Docs updated if behavior, config, or an API surface changed

## Architecture rules

Tick the ones this PR touches, or delete the section if none apply.

- [ ] Lifecycle stays in `internal/node` — nothing else spawns capability
      goroutines
- [ ] New capability implements the `Capability` interface in its own
      package and does not import `internal/node`
- [ ] `internal/memory` does not touch the pairing key or LAN-serving
      packages
- [ ] Background loops take a `context.Context` and return on cancellation
- [ ] New credential paths fail closed

## Control-plane contract

<!-- If this changes a request or response shared with the backend, link the
     backend change it tracks. Delete if not applicable. -->
