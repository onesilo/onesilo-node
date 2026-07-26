package lanserve

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/onesilo/silo-node/internal/pairing"
)

const testKID = "silo-assert-1"

// signES256 mints a compact ES256 JWT (test-local; mirrors the control plane).
func signES256(t *testing.T, key *ecdsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(map[string]any{"alg": "ES256", "kid": testKID, "typ": "JWT"}) + "." + enc(claims)
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

// pairTestRig wires a router with automated pairing enabled and returns the
// pieces a test needs to drive the app side of the handshake.
type pairTestRig struct {
	r             *Router
	s             *Session
	cap           *frameCapture
	pairer        *Pairer
	signKey       *ecdsa.PrivateKey
	nodeID        *ecdh.PrivateKey
	appID         *ecdh.PrivateKey
	appEph        *ecdh.PrivateKey
	appNonce      []byte
	assertionOnce string // memoized: ECDSA signing is randomized, must be stable
}

func newPairTestRig(t *testing.T, compute ComputeBackend, requireVerify bool) *pairTestRig {
	t.Helper()
	signKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nodeID, _ := pairing.GenerateEphemeral()
	appID, _ := pairing.GenerateEphemeral()
	appEph, _ := pairing.GenerateEphemeral()
	nonce, _ := pairing.NewNonce()

	keys := pairing.ECPublicKeys{testKID: &signKey.PublicKey}
	newResponder := func() *pairing.Responder {
		return pairing.NewResponder(nodeID, &pairing.AssertionVerifier{
			OwnDeviceID: "node-1",
			OwnAccount:  "acct-1",
			Keys:        pairing.StaticKeys(keys),
			Now:         func() time.Time { return time.Unix(1_700_000_000, 0) },
		})
	}
	pins, err := pairing.LoadPinStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pairer := NewPairer(newResponder, pins, requireVerify, slog.New(slog.DiscardHandler))

	// No file key: a nil file-key path proves the encrypted traffic below is
	// keyed purely by the handshake-derived session key.
	r := NewRouter(func() []byte { return nil }, compute, slog.New(slog.DiscardHandler))
	r.SetPairer(pairer)
	cap := &frameCapture{}
	return &pairTestRig{
		r: r, s: NewSession(cap.write), cap: cap, pairer: pairer,
		signKey: signKey, nodeID: nodeID, appID: appID, appEph: appEph, appNonce: nonce,
	}
}

func (rig *pairTestRig) appIDPubB64() string {
	return base64.StdEncoding.EncodeToString(rig.appID.PublicKey().Bytes())
}

func (rig *pairTestRig) assertion(t *testing.T) string {
	if rig.assertionOnce == "" {
		rig.assertionOnce = signES256(t, rig.signKey, map[string]any{
			"aud":            pairing.PairingAudience,
			"iat":            1_700_000_000,
			"exp":            1_700_000_060,
			"node_device_id": "node-1",
			"account_id":     "acct-1",
			"app_id_pub":     rig.appIDPubB64(),
		})
	}
	return rig.assertionOnce
}

func (rig *pairTestRig) hello(t *testing.T) []byte {
	h := pairing.Hello{
		Type:      pairing.FrameHello,
		AppIDPub:  rig.appIDPubB64(),
		AppEphPub: base64.StdEncoding.EncodeToString(rig.appEph.PublicKey().Bytes()),
		AppNonce:  base64.StdEncoding.EncodeToString(rig.appNonce),
		Assertion: rig.assertion(t),
	}
	b, _ := json.Marshal(h)
	return b
}

// completeHandshake drives hello+confirm and returns the derived session key
// (app side) so the test can encrypt traffic under it.
func (rig *pairTestRig) completeHandshake(t *testing.T) []byte {
	t.Helper()
	rig.r.Handle(context.Background(), rig.s, rig.hello(t))

	// Parse the pair_ack.
	var ack pairing.Ack
	if err := json.Unmarshal(rig.cap.frame(rig.cap.count()-1), &ack); err != nil || ack.Type != pairing.FrameAck {
		t.Fatalf("expected pair_ack, got %s", rig.cap.frame(rig.cap.count()-1))
	}
	nodeIDPub, _ := pairing.ParsePublicKey(mustB64(t, ack.NodeIDPub))
	nodeEphPub, _ := pairing.ParsePublicKey(mustB64(t, ack.NodeEphPub))
	nodeNonce := mustB64(t, ack.NodeNonce)

	assertionSHA := sha256.Sum256([]byte(rig.assertion(t)))
	tr := pairing.Transcript{
		AppIDPub:     rig.appID.PublicKey().Bytes(),
		NodeIDPub:    nodeIDPub.Bytes(),
		AppEphPub:    rig.appEph.PublicKey().Bytes(),
		NodeEphPub:   nodeEphPub.Bytes(),
		AppNonce:     rig.appNonce,
		NodeNonce:    nodeNonce,
		AssertionSHA: assertionSHA[:],
	}
	th := tr.Hash()
	ee, _ := rig.appEph.ECDH(nodeEphPub)
	ss, _ := rig.appID.ECDH(nodeIDPub)
	key, err := pairing.DeriveSessionKey(ee, ss, th)
	if err != nil {
		t.Fatal(err)
	}

	confirm := pairing.Confirm{Type: pairing.FrameConfirm, AppConfirm: base64.StdEncoding.EncodeToString(pairing.AppConfirm(key, th))}
	cb, _ := json.Marshal(confirm)
	rig.r.Handle(context.Background(), rig.s, cb)
	return key
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPairingEndToEndVerifiedTrafficFlows(t *testing.T) {
	compute := &fakeCompute{enabled: true, stream: scriptedStream([]string{"hello", " world"}, 3)}
	rig := newPairTestRig(t, compute, true)

	key := rig.completeHandshake(t)

	// First contact under requireVerify: pair_result must report unverified
	// and carry the SAS.
	var res pairResult
	json.Unmarshal(rig.cap.frame(rig.cap.count()-1), &res)
	if res.Type != "pair_result" || res.Verified || res.SAS == "" {
		t.Fatalf("expected unverified pair_result with SAS, got %+v", res)
	}

	// An encrypted user_message before verification is gated.
	before := rig.cap.count()
	rig.r.Handle(context.Background(), rig.s, encryptInner(t, key, "u1", map[string]any{"type": "user_message", "content": "hi"}))
	waitFor(t, func() bool { return rig.cap.count() > before })
	gated := rig.cap.decodeAll(t, key)
	last := gated[len(gated)-1]
	if !last.encrypted || last.msg["code"] != CodePairingUnverified {
		t.Fatalf("expected encrypted PAIRING_UNVERIFIED, got %+v", last)
	}

	// Operator confirms the SAS out of band (account is acct-1 in this rig).
	if err := rig.pairer.Verify("acct-1", rig.appIDPubB64()); err != nil {
		t.Fatal(err)
	}

	// Now a user_message runs to completion.
	before = rig.cap.count()
	rig.r.Handle(context.Background(), rig.s, encryptInner(t, key, "u2", map[string]any{"type": "user_message", "content": "hi again"}))
	waitFor(t, func() bool {
		for _, d := range rig.cap.decodeAll(t, key) {
			if d.msg["type"] == "completed" {
				return true
			}
		}
		return false
	})
	rig.r.Wait()
}

func TestPairingNoDowngradeToFileKey(t *testing.T) {
	rig := newPairTestRig(t, nil, true)
	// Start a handshake (hello only), then send an encrypted frame before the
	// session key exists: it must NOT fall back to any file key.
	rig.r.Handle(context.Background(), rig.s, rig.hello(t))
	before := rig.cap.count()
	// Encrypt under an arbitrary key; the router must refuse (no session key
	// yet, and no downgrade), returning a plain NO_KEY error.
	arbitrary := make([]byte, 32)
	rig.r.Handle(context.Background(), rig.s, encryptInner(t, arbitrary, "u1", map[string]any{"type": "health_check", "content": "x"}))
	if rig.cap.count() <= before {
		t.Fatal("expected a response frame")
	}
	var m map[string]any
	json.Unmarshal(rig.cap.frame(rig.cap.count()-1), &m)
	if m["code"] != CodeNoKey {
		t.Fatalf("expected NO_KEY (no downgrade), got %v", m)
	}
}

func TestPairingRejectsAssertionForDifferentAccount(t *testing.T) {
	rig := newPairTestRig(t, nil, true)
	// Assertion for a different account than the node pinned: rejected with a
	// pair_error (cross-tenant confused-deputy defense).
	bad := signES256(t, rig.signKey, map[string]any{
		"aud":            pairing.PairingAudience,
		"iat":            1_700_000_000,
		"exp":            1_700_000_060,
		"node_device_id": "node-1",
		"account_id":     "acct-2", // wrong tenant
		"app_id_pub":     rig.appIDPubB64(),
	})
	h := pairing.Hello{
		Type:      pairing.FrameHello,
		AppIDPub:  rig.appIDPubB64(),
		AppEphPub: base64.StdEncoding.EncodeToString(rig.appEph.PublicKey().Bytes()),
		AppNonce:  base64.StdEncoding.EncodeToString(rig.appNonce),
		Assertion: bad,
	}
	b, _ := json.Marshal(h)
	rig.r.Handle(context.Background(), rig.s, b)
	var m map[string]any
	json.Unmarshal(rig.cap.frame(rig.cap.count()-1), &m)
	if m["type"] != pairing.FrameError {
		t.Fatalf("expected pair_error for cross-account assertion, got %v", m)
	}
}
