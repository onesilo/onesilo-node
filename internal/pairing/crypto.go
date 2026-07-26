// Package pairing implements the node side of automated device pairing: an
// authenticated ECDH handshake that establishes the WebSocket E2E session
// key without a QR scan, for control-plane-relayed connections. See
// docs/automated-pairing.md.
//
// This file holds the pure cryptographic core — no I/O, no wire format —
// so it is unit-testable in isolation and shared by both handshake roles.
// Construction: static-static ECDH (identity keys, authenticate) mixed with
// ephemeral-ephemeral ECDH (forward secrecy), keyed by HKDF over a hash of
// the full handshake transcript, then an HMAC key-confirmation step.
package pairing

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

const (
	// kdfInfo domain-separates this protocol's session keys.
	kdfInfo = "onesilo-pairing-v1"
	// SessionKeyLen is the AES-256 key size the envelope expects.
	SessionKeyLen = 32
	// NonceLen is the handshake nonce size each side contributes.
	NonceLen = 16

	confirmLabelApp  = "onesilo-pairing-confirm-app"
	confirmLabelNode = "onesilo-pairing-confirm-node"
	sasInfo          = "onesilo-pairing-sas"
)

// Curve is the shared ECDH curve for identity and ephemeral keys. P-256 is
// chosen so the app's private identity key can be Secure-Enclave-resident.
func Curve() ecdh.Curve { return ecdh.P256() }

// GenerateEphemeral returns a fresh per-connection P-256 keypair (forward
// secrecy: it is discarded when the connection ends).
func GenerateEphemeral() (*ecdh.PrivateKey, error) {
	return Curve().GenerateKey(rand.Reader)
}

// NewNonce returns a fresh random handshake nonce.
func NewNonce() ([]byte, error) {
	n := make([]byte, NonceLen)
	if _, err := rand.Read(n); err != nil {
		return nil, fmt.Errorf("generating handshake nonce: %w", err)
	}
	return n, nil
}

// ParsePublicKey validates and decodes an uncompressed P-256 public key.
func ParsePublicKey(b []byte) (*ecdh.PublicKey, error) {
	return Curve().NewPublicKey(b)
}

// Transcript binds every public handshake value into one hash that feeds
// the KDF and the confirmation MACs, so any substituted key/nonce or
// replayed assertion produces a different session key (and fails
// confirmation) rather than silently succeeding.
type Transcript struct {
	AppIDPub     []byte // app long-term identity public key
	NodeIDPub    []byte // node long-term identity public key
	AppEphPub    []byte // app per-connection ephemeral public key
	NodeEphPub   []byte // node per-connection ephemeral public key
	AppNonce     []byte
	NodeNonce    []byte
	AssertionSHA []byte // SHA-256 of the raw control-plane assertion bytes
}

// Hash returns the length-prefixed SHA-256 of the transcript fields in a
// fixed order. Length-prefixing removes any field-boundary ambiguity so two
// distinct field splits can never collide.
func (t Transcript) Hash() []byte {
	h := sha256.New()
	for _, f := range [][]byte{
		t.AppIDPub, t.NodeIDPub, t.AppEphPub, t.NodeEphPub,
		t.AppNonce, t.NodeNonce, t.AssertionSHA,
	} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(f)))
		h.Write(n[:])
		h.Write(f)
	}
	return h.Sum(nil)
}

// DeriveSessionKey mixes the ephemeral-ephemeral and static-static shared
// secrets (ee first, then ss) and binds the transcript hash as the HKDF
// salt. Both handshake roles compute an identical key from the same public
// transcript; the control plane, seeing only public keys, cannot.
func DeriveSessionKey(eeShared, ssShared, transcriptHash []byte) ([]byte, error) {
	ikm := make([]byte, 0, len(eeShared)+len(ssShared))
	ikm = append(ikm, eeShared...)
	ikm = append(ikm, ssShared...)
	key, err := hkdf.Key(sha256.New, ikm, transcriptHash, kdfInfo, SessionKeyLen)
	if err != nil {
		return nil, fmt.Errorf("deriving session key: %w", err)
	}
	return key, nil
}

// ConfirmMAC is the key-confirmation tag one side sends to prove it derived
// the same session key over the same transcript. label distinguishes the
// app's tag from the node's so they can never be reflected.
func ConfirmMAC(sessionKey []byte, label string, transcriptHash []byte) []byte {
	m := hmac.New(sha256.New, sessionKey)
	m.Write([]byte(label))
	m.Write(transcriptHash)
	return m.Sum(nil)
}

// AppConfirm / NodeConfirm are the two directions of key confirmation.
func AppConfirm(sessionKey, transcriptHash []byte) []byte {
	return ConfirmMAC(sessionKey, confirmLabelApp, transcriptHash)
}

func NodeConfirm(sessionKey, transcriptHash []byte) []byte {
	return ConfirmMAC(sessionKey, confirmLabelNode, transcriptHash)
}

// VerifyMAC is a constant-time tag comparison.
func VerifyMAC(want, got []byte) bool { return hmac.Equal(want, got) }

// SAS derives the 8-digit Short Authentication String from the session key
// for one-time human verification on the first relayed pairing. Because it
// is computed over the derived key (not the raw static public keys), it also
// detects ephemeral substitution and downgrade, not just identity swaps.
func SAS(sessionKey []byte) (string, error) {
	raw, err := hkdf.Key(sha256.New, sessionKey, nil, sasInfo, 8)
	if err != nil {
		return "", fmt.Errorf("deriving SAS: %w", err)
	}
	n := binary.BigEndian.Uint64(raw) % 100_000_000
	return fmt.Sprintf("%08d", n), nil
}
