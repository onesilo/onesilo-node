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

### Primitives

- **ECDH: P-256** (`crypto/ecdh.P256` on the node; `SecureEnclave.P256.KeyAgreement`
  on iOS). P-256 is chosen over X25519 specifically so the app's private key
  can be **Secure Enclave–resident** (the Enclave does not hold X25519 keys).
  The node's private key is a `0600` file like its other secrets.
- **KDF: HKDF-SHA256.**
- **AEAD: AES-256-GCM** — the existing envelope `Seal`/`Open`, unchanged.

### Identity keys

- **Node identity key** — a P-256 keypair generated on first start and
  persisted at `<data_dir>/device_identity.key` (`0600`), alongside
  `node.key` / `memory.key`. The **public** key is published to the control
  plane in the destination registration.
- **App identity key** — a P-256 keypair with the private key in the Secure
  Enclave; the public key registered with the control plane, bound to the
  Clerk-authenticated user/device.

### Session key derivation

```
shared      = ECDH(local_priv, peer_pub)                 # 32-byte P-256 x-coord
session_key = HKDF-SHA256(
                ikm  = shared,
                salt = sort(node_device_id, app_device_id) joined,
                info = "onesilo-pairing-v1",
                len  = 32,
              )
```

`session_key` feeds the existing `Seal`/`Open` directly. Both sides derive
the identical key; the control plane, having only ever seen public keys,
cannot.

### Handshake (over the relay)

The control plane issues the app a short-lived **pairing assertion** —
signed with the control plane's key (verifiable via the existing OAuth JWKS)
— binding the app's public key to the target node for the owning account:

```
assertion = sign_cp({ app_pubkey, node_device_id, account_id, iat, exp })
```

The app already learns the node's public key from destination discovery
(an authenticated control-plane call). The WS handshake, before the
encrypted phase, adds two plaintext frames:

```
app → node : {"type":"pair_hello","app_pubkey":"<b64>","assertion":"<jwt>","nonce":"<b64>"}
node → app : {"type":"pair_ack","node_pubkey":"<b64>","nonce":"<b64>"}
```

The node verifies the assertion (control-plane signature via JWKS; `exp`;
that `node_device_id` is itself) — it already trusts the control plane
because it is signed in to it — then does `ECDH(node_priv, app_pubkey)`.
From that point both sides speak the existing AES-256-GCM envelope, keyed by
the derived `session_key`. No per-connection node→control-plane round trip
is needed: the signed assertion *is* the attestation.

### Per-connection keys

Today one global `pairing.key` keys every message. With ECDH each
connection derives its **own** session key from the peer's public key during
the handshake and holds it on the session object. This is strictly better —
a compromised app key exposes only that device's sessions, not all of them.
The node selects the key path per connection:

- `pair_hello` present → verify + ECDH → per-connection session key.
- no `pair_hello` → fall back to the stored `pairing.key` (LAN QR path,
  unchanged).

## Trust model

| Adversary | Can decrypt? | Why |
|---|---|---|
| Passive control plane / relay / tunnel | **No** | never sees the session key; only public keys pass through |
| Network attacker on the tunnel | **No** | payloads are E2E AES-256-GCM |
| **Active/malicious control plane** | Only by MITM | it would have to substitute public keys / forge an assertion — see below |

The residual risk is an **active** control plane substituting public keys.
Two mitigations, matching how Signal/Matrix/WhatsApp handle multi-device:

- **TOFU key pinning** (default, automatic): each side pins the peer's
  identity public key on first sight; a later change forces re-verification.
- **Optional verification code** (off by default): a Short Authentication
  String derived from both public keys —
  `SAS = base10(SHA256(sort(node_pub, app_pub)))[:8]` — shown in both the
  app and the node's admin UI for a one-time comparison. Matching SASs prove
  no MITM. Turning this on gives provable zero-trust-against-the-relay.

Without verification the guarantee is "as strong as TLS + an
account-attested identity key" — i.e. trust the identity provider not to
*actively* attack you. That is the industry-standard posture and a large
improvement over today's remote story (an operator hand-copying a symmetric
key, with no protection and no UX).

## What changes where

### onesilo-node

- Generate/persist the P-256 identity key (`<data_dir>/device_identity.key`,
  `0600`); expose the public key.
- `controlplane.RegisterRequest` gains `identity_pubkey`.
- lanserve handshake: accept `pair_hello`, verify the assertion against the
  control-plane JWKS, ECDH + HKDF → per-connection session key stored on the
  session; emit `pair_ack`. Fall back to `pairing.key` when absent.
- Session/router: key becomes per-connection instead of a single file key.
- Optional: surface the node's identity fingerprint / SAS in the admin UI
  Settings page for verification.
- No new setup step required — provisioning is automatic (the identity key
  is generated like `node.key`).

### onesilo-backend (control plane)

- Store per-account device identity public keys (node destinations + app
  devices); return the node's `identity_pubkey` in destination discovery.
- New endpoint: issue a signed **pairing assertion** to an authenticated app
  for a node it owns (`{app_pubkey, node_device_id, account_id, exp}`).
- Publish the assertion signing key via the existing OAuth JWKS surface so
  nodes can verify without new trust anchors.

### onesilo-apple

- Generate the P-256 identity key in the Secure Enclave; register the public
  key with the control plane.
- Fetch the node's public key + request a pairing assertion; ECDH + HKDF;
  use the derived key with the existing envelope crypto.
- Optional SAS verification UI + identity-key pinning ("safety number
  changed" on mismatch).

## Compatibility

Additive and non-breaking. The envelope wire format is unchanged
(AES-256-GCM combined). The LAN QR flow and `POST /v1/auth/pairing-key`
remain as the fallback / zero-trust option. New: two plaintext handshake
frames (`pair_hello` / `pair_ack`) ahead of the encrypted phase, and an
`identity_pubkey` field on destination registration. HKDF `info` carries a
version string (`onesilo-pairing-v1`) and `pair_hello` a protocol version
for future changes.

## Open questions

- Assertion lifetime and whether to bind it to the specific WS connection
  (e.g. include the app-chosen `nonce`) to prevent replay across connections.
- Whether to also upgrade the LAN path to ECDH (dropping QR entirely) or
  keep QR as the deliberate zero-trust bootstrap.
- Key rotation: reissuing a node identity key (and the "safety number
  changed" UX that follows) if `device_identity.key` is lost.
