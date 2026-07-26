package pairing

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

// appSide is a minimal in-test implementation of the app half of the
// handshake: it produces pair_hello, validates pair_ack (including the node's
// key-confirmation MAC), and produces pair_confirm. It derives the session
// key independently, so a matching key across both halves is what proves the
// protocol agrees.
type appSide struct {
	id        *ecdh.PrivateKey
	eph       *ecdh.PrivateKey
	nonce     []byte
	assertion string

	sessionKey []byte
	sas        string
}

func newAppSide(t *testing.T, assertion string) *appSide {
	t.Helper()
	id, err := GenerateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	eph, err := GenerateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	return &appSide{id: id, eph: eph, nonce: nonce, assertion: assertion}
}

func (a *appSide) hello() *Hello {
	return &Hello{
		Type:      FrameHello,
		AppIDPub:  base64.StdEncoding.EncodeToString(a.id.PublicKey().Bytes()),
		AppEphPub: base64.StdEncoding.EncodeToString(a.eph.PublicKey().Bytes()),
		AppNonce:  base64.StdEncoding.EncodeToString(a.nonce),
		Assertion: a.assertion,
	}
}

// processAck derives the app-side session key from the node's ack and checks
// the node's confirmation MAC. It returns the app's pair_confirm.
func (a *appSide) processAck(t *testing.T, ack *Ack) *Confirm {
	t.Helper()
	nodeIDPub := mustDecodePub(t, ack.NodeIDPub)
	nodeEphPub := mustDecodePub(t, ack.NodeEphPub)
	nodeNonce := mustDecodeB64(t, ack.NodeNonce)

	assertionSHA := sha256.Sum256([]byte(a.assertion))
	tr := Transcript{
		AppIDPub:     a.id.PublicKey().Bytes(),
		NodeIDPub:    nodeIDPub.Bytes(),
		AppEphPub:    a.eph.PublicKey().Bytes(),
		NodeEphPub:   nodeEphPub.Bytes(),
		AppNonce:     a.nonce,
		NodeNonce:    nodeNonce,
		AssertionSHA: assertionSHA[:],
	}
	th := tr.Hash()

	ee, err := a.eph.ECDH(nodeEphPub)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := a.id.ECDH(nodeIDPub)
	if err != nil {
		t.Fatal(err)
	}
	key, err := DeriveSessionKey(ee, ss, th)
	if err != nil {
		t.Fatal(err)
	}
	a.sessionKey = key
	if a.sas, err = SAS(key); err != nil {
		t.Fatal(err)
	}

	if !VerifyMAC(NodeConfirm(key, th), mustDecodeB64(t, ack.NodeConfirm)) {
		t.Fatal("node confirmation MAC did not verify on the app side")
	}
	return &Confirm{
		Type:       FrameConfirm,
		AppConfirm: base64.StdEncoding.EncodeToString(AppConfirm(key, th)),
	}
}

func mustDecodeB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decoding %q: %v", s, err)
	}
	return b
}

func mustDecodePub(t *testing.T, s string) *ecdh.PublicKey {
	t.Helper()
	pub, err := ParsePublicKey(mustDecodeB64(t, s))
	if err != nil {
		t.Fatalf("parsing pubkey: %v", err)
	}
	return pub
}

// assertionFor builds a valid assertion binding appIDPub to node-1/acct-1,
// signed by the verifier's trusted key.
func assertionFor(t *testing.T, key *ecdsa.PrivateKey, appIDPubB64 string) string {
	t.Helper()
	claims := baseClaims()
	claims["app_id_pub"] = appIDPubB64
	return signES256(t, key, "ES256", testKID, claims)
}

func TestHandshakeHappyPath(t *testing.T) {
	v, signKey := newVerifier(t)
	node, err := GenerateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	r := NewResponder(node, v)

	app := newAppSide(t, "") // assertion filled once we know the app id pub
	app.assertion = assertionFor(t, signKey, app.hello().AppIDPub)

	ack, err := r.Hello(context.Background(), app.hello())
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	confirm := app.processAck(t, ack)
	res, err := r.Confirm(confirm)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	if string(res.SessionKey) != string(app.sessionKey) {
		t.Fatal("node and app derived different session keys")
	}
	if res.SAS != app.sas {
		t.Fatalf("SAS mismatch: node %q app %q", res.SAS, app.sas)
	}
	if res.AccountID != "acct-1" || res.NodeDeviceID != "node-1" {
		t.Fatalf("unexpected bound identity: %+v", res)
	}
	if res.AppIDPubB64 != app.hello().AppIDPub {
		t.Fatal("bound app id pub does not match the presented key")
	}
}

func TestHandshakeRejectsAssertionForDifferentAppKey(t *testing.T) {
	v, signKey := newVerifier(t)
	node, _ := GenerateEphemeral()
	r := NewResponder(node, v)

	app := newAppSide(t, "")
	// Assertion attests a *different* app key than the one presented in hello.
	other, _ := GenerateEphemeral()
	otherPubB64 := base64.StdEncoding.EncodeToString(other.PublicKey().Bytes())
	app.assertion = assertionFor(t, signKey, otherPubB64)

	if _, err := r.Hello(context.Background(), app.hello()); err == nil {
		t.Fatal("expected rejection: assertion app_id_pub != hello app_id_pub")
	}
}

func TestHandshakeRejectsBadConfirm(t *testing.T) {
	v, signKey := newVerifier(t)
	node, _ := GenerateEphemeral()
	r := NewResponder(node, v)

	app := newAppSide(t, "")
	app.assertion = assertionFor(t, signKey, app.hello().AppIDPub)

	ack, err := r.Hello(context.Background(), app.hello())
	if err != nil {
		t.Fatal(err)
	}
	app.processAck(t, ack)
	// Tamper: an all-zero confirm MAC must not verify.
	bad := &Confirm{Type: FrameConfirm, AppConfirm: base64.StdEncoding.EncodeToString(make([]byte, 32))}
	if _, err := r.Confirm(bad); err == nil {
		t.Fatal("expected rejection of an invalid app confirmation MAC")
	}
}

func TestHandshakeRejectsUnsignedAssertion(t *testing.T) {
	v, _ := newVerifier(t)
	node, _ := GenerateEphemeral()
	r := NewResponder(node, v)

	app := newAppSide(t, "")
	// Sign with an untrusted key: verification must fail.
	untrusted, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	app.assertion = assertionFor(t, untrusted, app.hello().AppIDPub)
	if _, err := r.Hello(context.Background(), app.hello()); err == nil {
		t.Fatal("expected rejection of an assertion signed by an untrusted key")
	}
}

func TestHandshakeConfirmBeforeHello(t *testing.T) {
	v, _ := newVerifier(t)
	node, _ := GenerateEphemeral()
	r := NewResponder(node, v)
	if _, err := r.Confirm(&Confirm{Type: FrameConfirm, AppConfirm: "AAAA"}); err == nil {
		t.Fatal("expected error: confirm before hello")
	}
}
