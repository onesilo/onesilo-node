package pairing

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// Wire frames for the pre-encryption handshake. All keys and nonces are
// standard-base64 (matching the envelope's Content encoding); the assertion
// is the raw compact-JWT string. These three plaintext frames precede the
// AES-256-GCM envelope traffic; see docs/automated-pairing.md.
const (
	FrameHello   = "pair_hello"
	FrameAck     = "pair_ack"
	FrameConfirm = "pair_confirm"
	// FrameError is a plaintext handshake failure ({"type":"pair_error"}),
	// distinct from the encrypted-envelope error frame.
	FrameError = "pair_error"
)

// Hello is app → node: the app's identity and ephemeral public keys, its
// nonce, and the control-plane pairing assertion. The assertion's app_id_pub
// claim MUST match AppIDPub — otherwise a valid assertion for one app key
// could be replayed to key-agree under a different (attacker) identity.
type Hello struct {
	Type      string `json:"type"`
	AppIDPub  string `json:"app_id_pub"`
	AppEphPub string `json:"app_eph_pub"`
	AppNonce  string `json:"app_nonce"`
	Assertion string `json:"assertion"`
}

// Ack is node → app: the node's identity and ephemeral public keys, its
// nonce, and the node's key-confirmation MAC. The app verifies NodeConfirm
// before trusting the derived key, closing key confirmation in this
// direction.
type Ack struct {
	Type        string `json:"type"`
	NodeIDPub   string `json:"node_id_pub"`
	NodeEphPub  string `json:"node_eph_pub"`
	NodeNonce   string `json:"node_nonce"`
	NodeConfirm string `json:"node_confirm"`
}

// Confirm is app → node: the app's key-confirmation MAC. The node verifies
// AppConfirm to complete mutual key confirmation before any traffic flows.
type Confirm struct {
	Type       string `json:"type"`
	AppConfirm string `json:"app_confirm"`
}

// PairError is a plaintext handshake error frame.
type PairError struct {
	Type  string `json:"type"`
	Error string `json:"error"`
}

// Result is the outcome of a completed handshake: the per-connection session
// key, the human-comparable SAS, and the identity the peer proved.
type Result struct {
	SessionKey   []byte
	SAS          string
	AppIDPubB64  string
	AccountID    string
	NodeDeviceID string
}

// Responder runs the node side of the pairing handshake for a single
// connection. It is a strict two-step state machine — Hello then Confirm —
// and holds per-connection ephemeral state; construct one per connection and
// never reuse it. Not safe for concurrent calls (a connection is serviced by
// one read loop).
type Responder struct {
	identity *ecdh.PrivateKey
	verifier *AssertionVerifier

	// established after Hello, consumed by Confirm.
	eph            *ecdh.PrivateKey
	transcriptHash []byte
	sessionKey     []byte
	sas            string
	appIDPub       string
	accountID      string
	nodeDeviceID   string
	helloDone      bool
	done           bool
}

// NewResponder builds a per-connection responder over the node's long-term
// identity key and the assertion verifier (which pins the node's own device
// id and account).
func NewResponder(identity *ecdh.PrivateKey, verifier *AssertionVerifier) *Responder {
	return &Responder{identity: identity, verifier: verifier}
}

// Hello processes the app's pair_hello, verifies the assertion, performs the
// authenticated ECDH (ephemeral-ephemeral mixed with static-static, bound to
// the full transcript), derives the session key and SAS, and returns the
// node's pair_ack. The session key is not yet usable until Confirm succeeds.
func (r *Responder) Hello(ctx context.Context, h *Hello) (*Ack, error) {
	if r.helloDone {
		return nil, errors.New("pair_hello already processed on this connection")
	}
	appIDPub, err := decodePub("app_id_pub", h.AppIDPub)
	if err != nil {
		return nil, err
	}
	appEphPub, err := decodePub("app_eph_pub", h.AppEphPub)
	if err != nil {
		return nil, err
	}
	appNonce, err := decodeB64("app_nonce", h.AppNonce)
	if err != nil {
		return nil, err
	}
	if len(appNonce) != NonceLen {
		return nil, fmt.Errorf("app_nonce must be %d bytes, got %d", NonceLen, len(appNonce))
	}
	if h.Assertion == "" {
		return nil, errors.New("pair_hello missing assertion")
	}

	assertion, err := r.verifier.Verify(ctx, h.Assertion)
	if err != nil {
		return nil, fmt.Errorf("pairing assertion rejected: %w", err)
	}
	// The assertion attests a *specific* app identity key. Bind it to the key
	// actually presented (and used for the static-static DH below): without
	// this check a valid assertion could be relayed verbatim while the
	// attacker substitutes their own app_id_pub for the key agreement.
	if !constantTimeStringEqual(assertion.AppIDPubB64, h.AppIDPub) {
		return nil, errors.New("assertion app_id_pub does not match pair_hello app_id_pub")
	}

	r.eph, err = GenerateEphemeral()
	if err != nil {
		return nil, err
	}
	nodeNonce, err := NewNonce()
	if err != nil {
		return nil, err
	}

	// The transcript binds every public value and the exact assertion bytes,
	// so any substituted key/nonce or a replayed assertion yields a different
	// key and fails confirmation rather than silently agreeing.
	assertionSHA := sha256.Sum256([]byte(h.Assertion))
	t := Transcript{
		AppIDPub:     appIDPub.Bytes(),
		NodeIDPub:    r.identity.PublicKey().Bytes(),
		AppEphPub:    appEphPub.Bytes(),
		NodeEphPub:   r.eph.PublicKey().Bytes(),
		AppNonce:     appNonce,
		NodeNonce:    nodeNonce,
		AssertionSHA: assertionSHA[:],
	}
	th := t.Hash()

	eeShared, err := r.eph.ECDH(appEphPub)
	if err != nil {
		return nil, fmt.Errorf("ephemeral ECDH: %w", err)
	}
	ssShared, err := r.identity.ECDH(appIDPub)
	if err != nil {
		return nil, fmt.Errorf("static ECDH: %w", err)
	}
	sessionKey, err := DeriveSessionKey(eeShared, ssShared, th)
	if err != nil {
		return nil, err
	}
	sas, err := SAS(sessionKey)
	if err != nil {
		return nil, err
	}

	r.transcriptHash = th
	r.sessionKey = sessionKey
	r.sas = sas
	r.appIDPub = assertion.AppIDPubB64
	r.accountID = assertion.AccountID
	r.nodeDeviceID = assertion.NodeDeviceID
	r.helloDone = true

	return &Ack{
		Type:        FrameAck,
		NodeIDPub:   base64.StdEncoding.EncodeToString(r.identity.PublicKey().Bytes()),
		NodeEphPub:  base64.StdEncoding.EncodeToString(r.eph.PublicKey().Bytes()),
		NodeNonce:   base64.StdEncoding.EncodeToString(nodeNonce),
		NodeConfirm: base64.StdEncoding.EncodeToString(NodeConfirm(sessionKey, th)),
	}, nil
}

// Confirm verifies the app's key-confirmation MAC and, on success, finalizes
// the handshake. After this returns without error, Result() is valid and the
// session key may be used for envelope traffic.
func (r *Responder) Confirm(c *Confirm) (*Result, error) {
	if !r.helloDone {
		return nil, errors.New("pair_confirm before pair_hello")
	}
	if r.done {
		return nil, errors.New("pair_confirm already processed")
	}
	got, err := decodeB64("app_confirm", c.AppConfirm)
	if err != nil {
		return nil, err
	}
	want := AppConfirm(r.sessionKey, r.transcriptHash)
	if !VerifyMAC(want, got) {
		return nil, errors.New("app key-confirmation MAC is invalid (possible MITM or key mismatch)")
	}
	r.done = true
	return &Result{
		SessionKey:   r.sessionKey,
		SAS:          r.sas,
		AppIDPubB64:  r.appIDPub,
		AccountID:    r.accountID,
		NodeDeviceID: r.nodeDeviceID,
	}, nil
}

// SAS returns the short authentication string once Hello has run (empty
// before). Exposed so the admin surface can show it for verification.
func (r *Responder) SAS() string { return r.sas }

func decodeB64(field, s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid base64: %w", field, err)
	}
	return b, nil
}

func decodePub(field, s string) (*ecdh.PublicKey, error) {
	b, err := decodeB64(field, s)
	if err != nil {
		return nil, err
	}
	pub, err := ParsePublicKey(b)
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid P-256 public key: %w", field, err)
	}
	return pub, nil
}

// constantTimeStringEqual compares two base64 strings without leaking the
// match position via timing. The values are public, but a constant-time
// compare here is cheap and keeps the "no data-dependent branch on a
// security check" habit uniform.
func constantTimeStringEqual(a, b string) bool {
	return VerifyMAC([]byte(a), []byte(b))
}
