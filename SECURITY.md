# Security Policy

## Reporting a vulnerability

**Do not open a public issue for a security vulnerability.**

Email **security@onesilo.com** with a description of the issue, the version
or commit affected, and — if you have one — a reproduction. We will confirm
receipt and keep you updated as we work through it. If you would like
credit in the release notes, say so and tell us how you want to be named.

Please give us a reasonable window to ship a fix before disclosing
publicly.

## Scope

This repository is the silo-node daemon. Findings in the One Silo control
plane, the iOS or Mac apps, or the web dashboard should go to the same
address — say which component you are reporting on.

Things we are particularly interested in:

- Anything that lets the control plane, a tunnel provider, or a network
  observer recover a LAN session key or read session plaintext. The
  pairing handshake is designed so that no party other than the two
  endpoints can — see [docs/automated-pairing.md](docs/automated-pairing.md).
- Anything that turns a transport secret into an authorization decision.
  Holding the pairing key, or reaching the node through a tunnel, must not
  by itself grant access to a silo.
- Recovering memory plaintext without the key at `data_dir/memory.key`,
  including through the FTS index, or decrypting a record under a different
  silo id.
- Reaching the admin API from off-host, or getting it to act without a
  valid token.
- Getting the gateway relay to forward to an unintended upstream, or to
  leak the node's control-plane credential to a LAN client.
- Path, method, or header handling in the relay that allows a confused
  deputy.

## Out of scope

- Findings that require an attacker who already has local access to the
  data directory. Its contents are secrets by design; the directory is
  `0700` and credential files are `0600`.
- The LAN server binding `0.0.0.0`. That is deliberate — it is how
  on-network devices reach the node. Report failures of the
  authentication on top of it, not the bind itself.
- Denial of service through sheer volume against a node you control.

## Supported versions

silo-node is pre-1.0 and moves quickly. Fixes land on `main`; there are no
backported release branches yet. Run a recent build.
