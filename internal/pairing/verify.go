package pairing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// PairingAudience is the `aud` claim a node requires in a pairing assertion.
const PairingAudience = "silo-node-pairing"

// clockSkew tolerates small clock differences on exp/iat checks.
const clockSkew = 30 * time.Second

// Assertion is a verified pairing assertion: the control plane's attested
// statement that AppIDPubB64 may pair with this node for AccountID.
type Assertion struct {
	AppIDPubB64  string
	NodeDeviceID string
	AccountID    string
}

// ECPublicKeys maps a JWK `kid` to a P-256 verification key.
type ECPublicKeys map[string]*ecdsa.PublicKey

// KeySource returns the control plane's current assertion-signing keys
// (from its JWKS). Implementations cache; verification fails closed if this
// errors or yields no key for the token's kid.
type KeySource interface {
	Keys(ctx context.Context) (ECPublicKeys, error)
}

// AssertionVerifier verifies an ES256 pairing-assertion JWT and enforces
// that its claims bind to *this* node. It is a net-new inbound verifier:
// the node was previously only an outbound token presenter, so this is the
// one place the node trusts the control plane's signature — hence ES256-only
// (no alg confusion), pinned to the JWKS `kid`, and fail-closed throughout.
type AssertionVerifier struct {
	// OwnDeviceID / OwnAccount are what the node registered as; the assertion
	// must match both (the account check closes a cross-tenant confused
	// deputy — a signature is not an authorization).
	OwnDeviceID string
	OwnAccount  string
	Keys        KeySource
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

func (v *AssertionVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// Verify checks the token's ES256 signature against the JWKS and that every
// binding claim matches this node. Returns the trusted Assertion or an error.
func (v *AssertionVerifier) Verify(ctx context.Context, token string) (*Assertion, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("assertion is not a compact JWT")
	}

	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeJSONSegment(parts[0], &hdr); err != nil {
		return nil, fmt.Errorf("assertion header: %w", err)
	}
	// ES256 only — never trust `alg` from the token to pick the algorithm
	// (that is the classic alg-confusion / alg:none pitfall).
	if hdr.Alg != "ES256" {
		return nil, fmt.Errorf("unexpected assertion alg %q (want ES256)", hdr.Alg)
	}
	if hdr.Kid == "" {
		return nil, errors.New("assertion header missing kid")
	}

	keys, err := v.Keys.Keys(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading assertion signing keys: %w", err)
	}
	pub := keys[hdr.Kid]
	if pub == nil {
		return nil, fmt.Errorf("no trusted assertion key for kid %q", hdr.Kid)
	}

	sig, err := b64urlDecode(parts[2])
	if err != nil {
		return nil, fmt.Errorf("assertion signature: %w", err)
	}
	if len(sig) != 64 {
		return nil, fmt.Errorf("ES256 signature must be 64 bytes, got %d", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return nil, errors.New("assertion signature is invalid")
	}

	var c struct {
		Aud          string `json:"aud"`
		Exp          int64  `json:"exp"`
		Iat          int64  `json:"iat"`
		NodeDeviceID string `json:"node_device_id"`
		AccountID    string `json:"account_id"`
		AppIDPub     string `json:"app_id_pub"`
	}
	if err := decodeJSONSegment(parts[1], &c); err != nil {
		return nil, fmt.Errorf("assertion claims: %w", err)
	}

	now := v.now()
	if c.Aud != PairingAudience {
		return nil, fmt.Errorf("assertion audience %q is not %q", c.Aud, PairingAudience)
	}
	if c.Exp == 0 || now.After(time.Unix(c.Exp, 0).Add(clockSkew)) {
		return nil, errors.New("assertion is expired")
	}
	if c.Iat != 0 && now.Add(clockSkew).Before(time.Unix(c.Iat, 0)) {
		return nil, errors.New("assertion is not yet valid")
	}
	if c.NodeDeviceID != v.OwnDeviceID {
		return nil, errors.New("assertion is for a different node")
	}
	if c.AccountID != v.OwnAccount {
		return nil, errors.New("assertion is for a different account")
	}
	if c.AppIDPub == "" {
		return nil, errors.New("assertion missing app_id_pub")
	}
	return &Assertion{
		AppIDPubB64:  c.AppIDPub,
		NodeDeviceID: c.NodeDeviceID,
		AccountID:    c.AccountID,
	}, nil
}

// ParseJWKS extracts the P-256 (ES256) verification keys from a JWKS
// document, keyed by kid. Non-EC / non-P-256 keys (e.g. the RS256 OAuth key
// published on the same endpoint) are ignored.
func ParseJWKS(doc []byte) (ECPublicKeys, error) {
	var parsed struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			Kid string `json:"kid"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		return nil, fmt.Errorf("parsing JWKS: %w", err)
	}
	out := ECPublicKeys{}
	for _, k := range parsed.Keys {
		if k.Kty != "EC" || k.Crv != "P-256" || k.Kid == "" {
			continue
		}
		x, err := b64urlDecode(k.X)
		if err != nil {
			continue
		}
		y, err := b64urlDecode(k.Y)
		if err != nil {
			continue
		}
		pub := &ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(x),
			Y:     new(big.Int).SetBytes(y),
		}
		out[k.Kid] = pub
	}
	return out, nil
}

// StaticKeys is a KeySource over a fixed key set (tests, or a
// caller-managed cache).
type StaticKeys ECPublicKeys

func (s StaticKeys) Keys(context.Context) (ECPublicKeys, error) {
	return ECPublicKeys(s), nil
}

func b64urlDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func decodeJSONSegment(seg string, v any) error {
	raw, err := b64urlDecode(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}
