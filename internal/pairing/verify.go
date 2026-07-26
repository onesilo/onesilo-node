package pairing

import (
	"context"
	"crypto/ecdh"
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

// KidRefresher is an optional KeySource capability: when a token references
// a kid absent from the cached set, the verifier calls KeysForKid once to
// let the source pick up a key rotation before failing closed. The source
// is responsible for rate-limiting to avoid a bad-kid fetch storm.
type KidRefresher interface {
	KeysForKid(ctx context.Context, kid string) (ECPublicKeys, error)
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
	if v.Keys == nil {
		return nil, errors.New("assertion verifier has no key source configured")
	}
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
		// The kid is unknown to the cached set — likely a control-plane key
		// rotation. If the source can refresh for a specific kid, give it one
		// chance before failing closed (the source rate-limits this itself).
		if kr, ok := v.Keys.(KidRefresher); ok {
			if refreshed, rerr := kr.KeysForKid(ctx, hdr.Kid); rerr == nil {
				pub = refreshed[hdr.Kid]
			}
		}
	}
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
		// Only EC P-256 keys are assertion-signing keys; other kinds (e.g.
		// the co-published RS256 OAuth key) are legitimately ignored.
		if k.Kty != "EC" || k.Crv != "P-256" {
			continue
		}
		// But a *malformed* EC P-256 entry is fail-closed, not skipped:
		// silently dropping it would resurface later as a confusing "no
		// trusted key for kid" and hide a real key-format/rotation error.
		if k.Kid == "" {
			return nil, errors.New("JWKS EC P-256 key is missing kid")
		}
		pub, err := ecP256FromXY(k.X, k.Y)
		if err != nil {
			return nil, fmt.Errorf("JWKS key %q: %w", k.Kid, err)
		}
		out[k.Kid] = pub
	}
	return out, nil
}

// ecP256FromXY builds and validates a P-256 public key from base64url JWK
// coordinates: it rejects wrong-length coordinates and, crucially, points
// that are not on the curve (an invalid-curve point must never become a
// trusted verification key).
func ecP256FromXY(xB64, yB64 string) (*ecdsa.PublicKey, error) {
	x, err := b64urlDecode(xB64)
	if err != nil {
		return nil, fmt.Errorf("bad x coordinate: %w", err)
	}
	y, err := b64urlDecode(yB64)
	if err != nil {
		return nil, fmt.Errorf("bad y coordinate: %w", err)
	}
	if len(x) != 32 || len(y) != 32 {
		return nil, fmt.Errorf("P-256 coordinates must be 32 bytes (got x=%d, y=%d)", len(x), len(y))
	}
	// Validate the point is on the curve by round-tripping through
	// crypto/ecdh's validating constructor (uncompressed SEC1 point).
	uncompressed := make([]byte, 0, 65)
	uncompressed = append(uncompressed, 0x04)
	uncompressed = append(uncompressed, x...)
	uncompressed = append(uncompressed, y...)
	if _, err := ecdh.P256().NewPublicKey(uncompressed); err != nil {
		return nil, fmt.Errorf("point is not on the P-256 curve: %w", err)
	}
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(x),
		Y:     new(big.Int).SetBytes(y),
	}, nil
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
