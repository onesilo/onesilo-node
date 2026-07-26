package pairing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

const testKID = "silo-assert-1"

func signES256(t *testing.T, key *ecdsa.PrivateKey, alg, kid string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(map[string]any{"alg": alg, "kid": kid, "typ": "JWT"}) + "." + enc(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func newVerifier(t *testing.T) (*AssertionVerifier, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v := &AssertionVerifier{
		OwnDeviceID: "node-1",
		OwnAccount:  "acct-1",
		Keys:        StaticKeys{testKID: &key.PublicKey},
		Now:         func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	return v, key
}

func baseClaims() map[string]any {
	return map[string]any{
		"aud":            PairingAudience,
		"iat":            1_700_000_000,
		"exp":            1_700_000_060,
		"node_device_id": "node-1",
		"account_id":     "acct-1",
		"app_id_pub":     "APP_PUBKEY_B64",
	}
}

func TestVerifyValidAssertion(t *testing.T) {
	v, key := newVerifier(t)
	token := signES256(t, key, "ES256", testKID, baseClaims())
	a, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("valid assertion rejected: %v", err)
	}
	if a.AppIDPubB64 != "APP_PUBKEY_B64" || a.NodeDeviceID != "node-1" || a.AccountID != "acct-1" {
		t.Fatalf("unexpected assertion: %+v", a)
	}
}

func TestVerifyRejects(t *testing.T) {
	cases := []struct {
		name   string
		alg    string
		kid    string
		mutate func(map[string]any)
	}{
		{"wrong audience", "ES256", testKID, func(c map[string]any) { c["aud"] = "evil" }},
		{"expired", "ES256", testKID, func(c map[string]any) { c["exp"] = 1_699_999_900 }},
		{"different node", "ES256", testKID, func(c map[string]any) { c["node_device_id"] = "node-2" }},
		{"different account", "ES256", testKID, func(c map[string]any) { c["account_id"] = "acct-2" }},
		{"missing app key", "ES256", testKID, func(c map[string]any) { delete(c, "app_id_pub") }},
		{"non-ES256 alg", "HS256", testKID, func(c map[string]any) {}},
		{"unknown kid", "ES256", "other-kid", func(c map[string]any) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, key := newVerifier(t)
			claims := baseClaims()
			tc.mutate(claims)
			token := signES256(t, key, tc.alg, tc.kid, claims)
			if _, err := v.Verify(context.Background(), token); err == nil {
				t.Fatalf("%s: expected rejection, got none", tc.name)
			}
		})
	}
}

func TestVerifyRejectsWrongSigner(t *testing.T) {
	v, _ := newVerifier(t)
	// A token signed by a *different* key with the trusted kid must fail:
	// the kid is pinned to the trusted public key, so signature verify fails.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	token := signES256(t, other, "ES256", testKID, baseClaims())
	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Fatal("assertion signed by an untrusted key was accepted")
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	v, key := newVerifier(t)
	token := signES256(t, key, "ES256", testKID, baseClaims())
	// Flip a byte in the payload segment; signature no longer matches.
	parts := []byte(token)
	dot := 0
	for i, b := range parts {
		if b == '.' {
			dot++
			if dot == 1 {
				parts[i+1] ^= 0x01 // corrupt the first payload byte
				break
			}
		}
	}
	if _, err := v.Verify(context.Background(), string(parts)); err == nil {
		t.Fatal("tampered assertion was accepted")
	}
}

func TestParseJWKSSelectsP256(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	x := base64.RawURLEncoding.EncodeToString(key.PublicKey.X.FillBytes(make([]byte, 32)))
	y := base64.RawURLEncoding.EncodeToString(key.PublicKey.Y.FillBytes(make([]byte, 32)))
	doc := []byte(`{"keys":[
		{"kty":"RSA","kid":"silo-oauth-1","alg":"RS256","n":"abc","e":"AQAB"},
		{"kty":"EC","crv":"P-256","kid":"silo-assert-1","alg":"ES256","x":"` + x + `","y":"` + y + `"}
	]}`)
	keys, err := ParseJWKS(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 EC key, got %d (RSA key should be ignored)", len(keys))
	}
	if keys["silo-assert-1"] == nil {
		t.Fatal("P-256 key not parsed under its kid")
	}
	// The parsed key must actually verify a token the original key signed.
	v := &AssertionVerifier{
		OwnDeviceID: "node-1", OwnAccount: "acct-1",
		Keys: StaticKeys(keys), Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	if _, err := v.Verify(context.Background(), signES256(t, key, "ES256", "silo-assert-1", baseClaims())); err != nil {
		t.Fatalf("parsed JWKS key failed to verify: %v", err)
	}
}
