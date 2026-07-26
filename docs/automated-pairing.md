# Automated pairing (remote E2E without QR)

**Status:** proposal · **Scope:** onesilo-node + onesilo-backend (+ onesilo-apple)

## Problem

The WebSocket LLM protocol (see [protocol.md](protocol.md)) is end-to-end
encrypted with a 32-byte AES-256-GCM **pairing key** shared out-of-band —
today by scanning a QR code. The key is pushed into the node's loopback
admin API (`POST /v1/auth/pairing-key`) and stored at
`<data_dir>/pairing.key`. The node is a **passive holder**: it never
generates or scans anything.

Two gaps follow:

1. **Headless nodes can't pair.** A standalone `silo-node` (no Silo Desktop
   driving it) has no way to establish a pairing key with an app. An
   operator would have to hand-generate 64 hex, `curl` it into the admin
   API, and separately get the same value into the app. There is no QR
   generation and no setup step.
2. **Remote pairing is manual.** When an app reaches a node over the
   control-plane relay (a tunnelled destination — see
   [Modes & remote access](../README.md#modes--remote-access)), there is no
   proximity for a QR scan, yet the key must still be shared out-of-band.

## Goal

Establish the session key **automatically** for control-plane-relayed
sessions, while keeping the property that makes this E2E: **the control
plane never learns the symmetric key.** Give headless nodes a real pairing
story. Keep the LAN QR + `pairing.key` path working unchanged as a fallback.

## Non-goal / the one hard rule

The control plane must **never** hold or distribute the symmetric session
key. If it did, the relay could decrypt every session and this would no
longer be end-to-end encrypted — it would just be TLS to the control plane.
Every design choice below preserves this.

## Design: authenticated ECDH, relayed blindly through the control plane

Replace the QR out-of-band channel with an ECDH key agreement between the
node and the app. The control plane acts as an **identity directory and
attestation authority** — it stores and vouches for each device's *public*
key — but never as key escrow.

> **Security review (2026-07):** the first draft of this protocol
> authenticated the *signed assertion* but not the *live peer or the
> channel*, and had no forward secrecy, revocation, or audit logging. This
> section has been revised to close those gaps; see
> [Security & SOC 2 considerations](#security--soc-2-considerations) for the
> finding-by-finding rationale. **Do not implement an earlier revision.**

### Primitives

- **Static ECDH: P-256** (`crypto/ecdh.P256` on the node;
  `SecureEnclave.P256.KeyAgreement` on iOS) for the long-term **identity**
  keys. P-256 is chosen over X25519 specifically so the app's private
  identity key can be **Secure Enclave–resident** (the Enclave does not hold
  X25519 keys).
- **Ephemeral ECDH: P-256**, a fresh keypair per connection on each side, for
  **forward secrecy**. The final key mixes the ephemeral-ephemeral and
  static-static shared secrets (a Noise-`IK`/X3DH-style construction): the
  static keys *authenticate*, the ephemerals give *PFS*, so stealing an
  identity key later does not decrypt recorded sessions. App ephemerals are
  disposable and need not be Enclave-resident.
- **KDF: HKDF-SHA256**, with the **full handshake transcript bound in** (both
  identity + ephemeral public keys, both nonces, the assertion hash).
- **AEAD: AES-256-GCM** — the existing envelope `Seal`/`Open`, unchanged.

### Identity keys

- **Node identity key** — a P-256 keypair generated on first start and
  persisted at `<data_dir>/device_identity.key` (`0600`), alongside
  `node.key` / `memory.key`. The **public** key is published to the control
  plane in the destination registration.
- **App identity key** — a P-256 keypair with the private key in the Secure
  Enclave; the public key registered with the control plane, bound to the
  Clerk-authenticated user/device.

### Handshake (over the relay)

The control plane issues the app a short-lived **pairing assertion** —
signed `ES256` with the control plane's dedicated pairing key (verifiable via
JWKS) — binding the app's identity public key to the target node **for the
account that owns the node**. Assertions are single-use-ish (`exp ≤ 60s`).

```
assertion = sign_cp({ app_id_pub, node_device_id, account_id, iat, exp, kid })
```

The app learns the node's identity public key from destination discovery (an
authenticated control-plane call). The handshake, before the encrypted
phase, is **three** plaintext frames — the node contributes a challenge so
the exchange is bound to *this* connection, and both sides prove key
possession before any application data:

```
app  → node : pair_hello  { app_id_pub, app_eph_pub, assertion, app_nonce }
node → app  : pair_ack    { node_id_pub, node_eph_pub, node_nonce }
app  → node : pair_confirm { mac_app  = HMAC(K, "app"  ‖ transcript) }
node → app  : (accept)     { mac_node = HMAC(K, "node" ‖ transcript) }
```

The node **must** verify, and reject + audit-log on any failure:
1. assertion signature (`ES256` only — reject `alg:none`/RS/HS confusion),
   `exp`, and pinned `kid` against cached JWKS (fail closed);
2. `assertion.node_device_id == own device_id`; and
3. **`assertion.account_id == the account this node is registered under`**
   (the node knows its own account from its control-plane credential — this
   is the authorization check, not just signature verification).

Both sides then derive:

```
transcript  = SHA256( app_id_pub ‖ node_id_pub ‖ app_eph_pub ‖ node_eph_pub
                       ‖ app_nonce ‖ node_nonce ‖ SHA256(assertion) )
ss          = ECDH(id_priv,  peer_id_pub)     # static-static: authenticates
ee          = ECDH(eph_priv, peer_eph_pub)    # ephemeral-ephemeral: PFS
session_key = HKDF-SHA256(ikm = ee ‖ ss, salt = transcript,
                          info = "onesilo-pairing-v1", len = 32)
```

`session_key` feeds the existing `Seal`/`Open` unchanged. The `pair_confirm`
MAC is the **key-confirmation** step: a MITM who substituted any public key
or replayed an assertion derives a different key, so its MAC fails and the
connection is refused (and logged as a tamper event) rather than silently
falling back to a wrong-key error. The control plane, having only ever seen
public keys, cannot derive `session_key`.

### Per-connection keys and no silent downgrade

Each connection derives its **own** `session_key` (held on the session
object) — strictly better than one global key: a compromised app key exposes
only that device's sessions. Path selection is by **transport**, not by
whichever key happens to exist:

- **Control-plane-relayed** connection → the node **requires** a valid
  `pair_hello` + assertion + confirmation. It **never** falls through to the
  shared `pairing.key`; a relayed connection that omits the strong path is
  refused and logged (this closes the downgrade attack where a MITM simply
  strips `pair_hello`).
- **LAN** connection → the QR-bootstrapped `pairing.key` path remains as the
  deliberate zero-trust fallback, unchanged. (It may also be upgraded to the
  ECDH handshake later — see Open questions.)

The app records that a node is reachable via ECDH and warns on any later
downgrade (downgrade pinning).

## Trust model

| Adversary | Can decrypt? | Why |
|---|---|---|
| Passive control plane / relay / tunnel | **No** | never sees the session key; only public keys cross it |
| Network attacker on the tunnel | **No** | payloads are E2E AES-256-GCM |
| Later theft of a long-term identity key | **No** for past sessions | ephemeral DH gives forward secrecy; recorded traffic stays sealed |
| **Active** control plane, established peer | No (detected) | key-confirmation + transcript-bound KDF + a *pinned* peer key make substitution fail and alarm |
| **Active** control plane, **first** contact | Only in the TOFU window | before a pin exists, the app learns the node key over the control plane's own channel — see below |

The one genuinely hard case is an **active** control plane at **first
contact**: there is no pin yet, and the node's public key is discovered over
the control plane itself. Mitigations, matching how Signal/Matrix/WhatsApp
handle multi-device:

- **TOFU key pinning** (always on): each side pins the peer's identity key on
  first sight; any later change forces re-verification ("safety number
  changed") and is audit-logged.
- **Short Authentication String**, computed over the **derived key /
  transcript** (not the raw static pubkeys, so it also catches ephemeral
  substitution and downgrade): `SAS = base10(HKDF(session_key, "sas"))[:8]`,
  shown in both the app and the node's admin UI. **Required on the first
  relayed pairing** (or bootstrap that first pairing over LAN/QR), optional
  thereafter. A one-time confirmation is the only thing that closes the
  first-contact window against an active provider — it is a deliberate,
  minimal user step, not an escape hatch.

After first contact the guarantee is strong (pinned keys + confirmation +
PFS); the first-contact step is where "fully automatic" and "provable
zero-trust-against-the-relay" trade off. **Decision: take the more secure
option — the one-time SAS is *required* on the first relayed pairing** (or
that first pairing is bootstrapped over LAN/QR). This closes the
active-control-plane first-contact window at the cost of a single one-time
confirmation; subsequent connections to a pinned peer are automatic. We do
**not** ship TOFU-only first contact.

## What changes where

### onesilo-node

- Generate/persist the P-256 **identity** key, **wrapped by the existing
  `memory.key` / OS keystore** rather than stored raw (`0700` dir); expose
  the public key. Generate an **ephemeral** P-256 keypair per connection.
- `controlplane.RegisterRequest` gains `identity_pubkey`; the node is the
  **only** principal allowed to set its own pubkey.
- **New inbound verifier** (net-new surface — the node has no inbound
  JWT/JWKS verifier today): validate assertions `ES256`-only against a pinned
  issuer/`kid`, cached JWKS, fail-closed.
- lanserve handshake: `pair_hello` → `pair_ack` → `pair_confirm`; verify
  signature + `exp` + `node_device_id` + **`account_id == own account`**;
  ephemeral+static ECDH; transcript-bound HKDF; **require** the strong path
  on relayed connections (no `pairing.key` fallback); key-confirmation MAC.
- Session/router: per-connection key; per-key message ceiling / rekey.
- Surface the node's SAS / identity fingerprint in the admin UI for
  verification; audit-log pair attempts, verify results, fallbacks, pin
  changes.

### onesilo-backend (control plane)

- Store per-account device identity public keys; return the node's pubkey in
  destination discovery. A node's pubkey is settable **only by that
  authenticated node**; app pubkeys bound to the owning Clerk user/device; a
  pubkey **change** is a step-up-gated, audit-logged security event.
- **Pairing-assertion endpoint**: least-privilege (only the account owner,
  scoped to nodes they own), rate-limited, `exp ≤ 60s`, opaque `account_id`.
- **Dedicated `ES256` signing key in HSM/KMS** (non-exportable, own `kid`,
  documented rotation with overlap), separate from the general OAuth key;
  published via JWKS.
- **Device revocation**: revoked app/node keys refuse new assertions; a short
  revocation feed nodes can poll.
- Ship all pairing/key events to the SOC 2 audit pipeline.

### onesilo-apple (iOS + SiloDesktop)

- Generate the P-256 identity key in the **Secure Enclave**; disposable
  ephemeral per connection; register the identity pubkey.
- Fetch the node's pubkey + request an assertion; run the handshake; use the
  derived key with the existing envelope crypto.
- SAS verification UI (required on first relayed pairing), identity-key
  pinning + "safety number changed" recovery, downgrade warning.

## Compatibility

Additive and non-breaking to the envelope wire format (AES-256-GCM combined).
The LAN QR flow and `POST /v1/auth/pairing-key` remain as the LAN fallback.
New: the `pair_hello`/`pair_ack`/`pair_confirm` handshake ahead of the
encrypted phase, an `identity_pubkey` on destination registration, and a
**downgrade-protected** version negotiated inside the transcript (so a
tampered plaintext version field is detected by key-confirmation).

## Security & SOC 2 considerations

An adversarial design review (2026-07) rewrote the protocol above. The
must-fixes are folded in; this section records the rationale and maps
controls to the SOC 2 Common Criteria. Findings are Fn.

**Blocking crypto/protocol fixes (now in the design):**

- **Key confirmation (F-1, CC6.1/CC7.2):** the `pair_confirm` MAC proves both
  sides derived the same key *before* trusting the channel, so an active
  substitution is detected and alarmed instead of surfacing as a generic
  decryption error.
- **Forward secrecy (F-2, CC6.1):** ephemeral-ephemeral DH mixed with the
  static-static DH. Theft of a long-term identity key no longer decrypts
  recorded sessions — essential given the node key is a file at rest (F-11).
- **Connection-bound assertion (F-3, CC6.1):** node-provided nonce + both
  nonces in the transcript + `exp ≤ 60s` + key-confirmation defeat replay and
  reflection; a captured assertion can't complete without the live app key.
- **No silent downgrade (F-4, CC6.1/CC6.6):** relayed connections require the
  ECDH path; stripping `pair_hello` is refused and logged, not accepted on
  the weaker `pairing.key`. Handshake frames are integrity-covered by the
  transcript MAC.
- **Transcript-bound KDF (F-5, CC6.1):** HKDF salt = hash of both identity +
  ephemeral pubkeys, both nonces, and the assertion — so any substituted key
  yields a different session key and fails confirmation.
- **Node enforces `account_id` (F-8, CC6.1/CC6.3):** the node checks the
  assertion's account against its *own* registered account — closing the
  confused-deputy / cross-tenant hole (signature ≠ authorization).

**Key management & operations (SOC 2):**

- **Signing-key custody (F-9, CC6.1/CC6.6/CC7.1):** dedicated `ES256` key in
  HSM/KMS, `kid`, rotation with overlap; node enforces an algorithm
  allow-list (reject `alg:none`, RS/HS confusion) and fails closed on JWKS.
- **Revocation & recovery (F-10, CC6.1/CC7.3/CC7.4):** device revocation
  list + short assertions so revocation propagates within one `exp`;
  "safety number changed" re-verification; identity keys are **never**
  escrowed/backed up (escrow would break E2E).
- **Node key at rest (F-11, CC6.1/CC6.7):** wrap `device_identity.key` with
  `memory.key`/OS keystore; the node host is in-scope for endpoint controls.
- **Audit logging (F-12, CC7.2/CC7.3):** structured events on server and
  client — assertion issuance, pubkey registration/**change**, pair
  success/failure, key-confirmation failure (candidate MITM), fallback used,
  pin change — to the audit pipeline, with alerts on the anomalies.
- **Access control & rate limits (F-13, CC6.1/CC6.3/CC6.6):** only a node may
  set its own pubkey; scoped, rate-limited assertion issuance; pubkey change
  gated + logged.
- **Crypto-agility (F-14, CC8.1):** downgrade-protected version negotiation
  and an algorithm deprecation path (room for X25519 / ML-KEM hybrid later).
- **Data minimization (F-15, CC6.1/C1):** opaque, rotating `account_id` in the
  assertion (no email/stable PII); assertions kept out of plaintext logs.
- **Availability (F-16, A1/CC7.2):** verify the (cheap, cached-JWKS)
  signature before the (expensive) ECDH; cap concurrent handshakes and
  rate-limit per source; never fetch JWKS synchronously per connection.

**Lower severity:** per-key AES-GCM message ceiling / rekey and keeping
`sealWithNonce` test-only (F-6); bind `user_id` as GCM AAD if it ever gains
meaning.

**What the review affirmed:** the core E2E invariant (control plane never
holds the symmetric key), per-connection keys, P-256-for-Secure-Enclave, and
TOFU+SAS are the right foundations — the fixes above harden *how* they're
composed.

## Decisions & open items

**Decided (take the more secure option):**

- **First-contact verification is required.** The one-time SAS must be
  confirmed on the first relayed pairing (or that first pairing bootstrapped
  over LAN/QR). No TOFU-only first contact. See the Trust model.
- **LAN keeps QR as its zero-trust bootstrap.** We do not route local
  pairing through the control plane — a pure out-of-band local QR is the
  more private, more-verified LAN path, and it remains the fallback.

**Still open (implementation detail, not security-blocking):**

- Node identity-key **rotation** UX and the re-pin ("safety number changed")
  flow when `device_identity.key` is lost or wrapped-key recovery fails —
  scope during the node PR.

## Implementation notes (node)

The node side is implemented in `internal/pairing` (pure protocol) and wired
into the WebSocket server in `internal/lanserve`. The build follows the
"take the more secure option" decisions above, so the shipped construction is
a hardened superset of the two-frame sketch earlier in this doc:

- **Three frames, mutually key-confirmed.** `pair_hello` (app→node),
  `pair_ack` (node→app, carries the node's confirmation MAC), `pair_confirm`
  (app→node, carries the app's confirmation MAC). Traffic only flows after
  both MACs verify — a key mismatch or MITM fails confirmation instead of
  silently agreeing to different keys.
- **Ephemeral + static ECDH.** Each side contributes a per-connection
  ephemeral P-256 key *and* its long-term identity key; the session key is
  `HKDF(ee ‖ ss, salt = H(transcript), info = "onesilo-pairing-v1")`. The
  ephemerals give forward secrecy (a later identity-key theft can't decrypt
  recorded sessions); the static keys authenticate.
- **Transcript binding.** Every public value — both identity keys, both
  ephemerals, both nonces, and the SHA-256 of the exact assertion bytes — is
  length-prefixed into the KDF salt and the confirmation MACs, so any
  substituted key/nonce or a replayed assertion yields a different key.
- **Assertion↔key binding.** The node verifies the control-plane assertion
  (ES256 over the OAuth JWKS, `kid`-pinned, `aud`/`exp` checked) *and* that
  its `app_id_pub` claim equals the identity key presented in `pair_hello`
  and used for the static DH. The assertion is also bound to *this* node's
  registered device id **and** `account_id` (captured at
  registration), closing a cross-tenant confused-deputy even against a
  validly signed assertion for another node/account.
- **Per-connection keys, no downgrade.** The derived key lives on the
  session, not in a shared file. Once a connection has begun a handshake it
  is handshake-only: it can never fall back to the legacy `pairing.key`.
- **TOFU pinning + required first-contact SAS.** App identity keys are pinned
  in `<data_dir>/paired_devices.json`. A first-contact key is held
  *unverified*: `user_message` inference is refused (`PAIRING_UNVERIFIED`)
  until the 8-digit SAS is confirmed in the node admin UI
  (`POST /v1/pairing/verify`; pending list at `GET /v1/pairing/pending`). A
  changed key for an already-verified account is a hard stop
  (`ErrSafetyNumberChanged`). Set `lan.require_pairing_verification = false`
  to trust-on-first-use without the manual SAS step.
- **JWKS fetch.** The node fetches the assertion-signing keys from the
  control plane's `/oauth/jwks`, caches them, serves a stale cache through a
  transient control-plane outage, and refreshes once (rate-limited) on an
  unknown `kid` so a key rotation is picked up without a restart.

The envelope wire format (AES-256-GCM combined) and the LAN QR / `pairing.key`
fallback are unchanged.
