package pairing

import (
	"bytes"
	"testing"
)

// handshake runs the full key agreement for both roles and returns the two
// derived session keys, mirroring what the app and node each compute.
func handshake(t *testing.T, tamper func(appT, nodeT *Transcript)) (appKey, nodeKey []byte) {
	t.Helper()
	appID, _ := GenerateEphemeral() // stand-ins for the long-term identity keys
	nodeID, _ := GenerateEphemeral()
	appEph, _ := GenerateEphemeral()
	nodeEph, _ := GenerateEphemeral()
	appNonce, _ := NewNonce()
	nodeNonce, _ := NewNonce()
	assertionSHA := bytes.Repeat([]byte{0xab}, 32)

	base := Transcript{
		AppIDPub:     appID.PublicKey().Bytes(),
		NodeIDPub:    nodeID.PublicKey().Bytes(),
		AppEphPub:    appEph.PublicKey().Bytes(),
		NodeEphPub:   nodeEph.PublicKey().Bytes(),
		AppNonce:     appNonce,
		NodeNonce:    nodeNonce,
		AssertionSHA: assertionSHA,
	}
	appT, nodeT := base, base
	if tamper != nil {
		tamper(&appT, &nodeT)
	}

	// App side: ee = ECDH(appEph, nodeEph); ss = ECDH(appID, nodeID).
	appEE, _ := appEph.ECDH(nodeEph.PublicKey())
	appSS, _ := appID.ECDH(nodeID.PublicKey())
	appKey, err := DeriveSessionKey(appEE, appSS, appT.Hash())
	if err != nil {
		t.Fatal(err)
	}
	// Node side: the crossed ECDH must yield the same shared secrets.
	nodeEE, _ := nodeEph.ECDH(appEph.PublicKey())
	nodeSS, _ := nodeID.ECDH(appID.PublicKey())
	nodeKey, err = DeriveSessionKey(nodeEE, nodeSS, nodeT.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return appKey, nodeKey
}

func TestHandshakeAgreesOnKey(t *testing.T) {
	appKey, nodeKey := handshake(t, nil)
	if !bytes.Equal(appKey, nodeKey) {
		t.Fatal("app and node derived different session keys")
	}
	if len(appKey) != SessionKeyLen {
		t.Fatalf("session key is %d bytes, want %d", len(appKey), SessionKeyLen)
	}

	// Key confirmation both ways.
	th := Transcript{}.Hash() // any shared transcript works for the MAC test
	if !VerifyMAC(AppConfirm(appKey, th), AppConfirm(nodeKey, th)) {
		t.Error("app confirmation MAC mismatch on equal keys")
	}
	if bytes.Equal(AppConfirm(appKey, th), NodeConfirm(nodeKey, th)) {
		t.Error("app and node confirmation MACs must differ (label separation)")
	}
}

func TestTranscriptMismatchBreaksKey(t *testing.T) {
	// A MITM that substitutes the node's ephemeral in the app's view (but
	// can't change what the node actually holds) yields divergent transcripts
	// → divergent keys → confirmation fails. This is the substitution defense.
	appKey, nodeKey := handshake(t, func(appT, nodeT *Transcript) {
		appT.NodeEphPub = bytes.Repeat([]byte{0x01}, len(appT.NodeEphPub))
	})
	if bytes.Equal(appKey, nodeKey) {
		t.Fatal("substituted transcript still produced matching keys")
	}
	th1 := Transcript{AppNonce: []byte("a")}.Hash()
	// The honest node would reject the app's confirmation (computed under a
	// different key) — model that as unequal MACs.
	if VerifyMAC(AppConfirm(appKey, th1), AppConfirm(nodeKey, th1)) {
		t.Error("confirmation MAC should not verify across mismatched keys")
	}
}

func TestTranscriptHashBindsEveryField(t *testing.T) {
	base := Transcript{
		AppIDPub: []byte("a"), NodeIDPub: []byte("b"),
		AppEphPub: []byte("c"), NodeEphPub: []byte("d"),
		AppNonce: []byte("e"), NodeNonce: []byte("f"),
		AssertionSHA: []byte("g"),
	}
	h0 := base.Hash()
	mutate := []func(*Transcript){
		func(x *Transcript) { x.AppIDPub = []byte("A") },
		func(x *Transcript) { x.NodeIDPub = []byte("B") },
		func(x *Transcript) { x.AppEphPub = []byte("C") },
		func(x *Transcript) { x.NodeEphPub = []byte("D") },
		func(x *Transcript) { x.AppNonce = []byte("E") },
		func(x *Transcript) { x.NodeNonce = []byte("F") },
		func(x *Transcript) { x.AssertionSHA = []byte("G") },
	}
	for i, m := range mutate {
		c := base
		m(&c)
		if bytes.Equal(h0, c.Hash()) {
			t.Errorf("field %d does not affect the transcript hash", i)
		}
	}
	// Length-prefixing: moving a byte across a field boundary must change it.
	amb1 := Transcript{AppIDPub: []byte("ab"), NodeIDPub: []byte("c")}
	amb2 := Transcript{AppIDPub: []byte("a"), NodeIDPub: []byte("bc")}
	if bytes.Equal(amb1.Hash(), amb2.Hash()) {
		t.Error("transcript hash is ambiguous across field boundaries")
	}
}

func TestSAS(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, SessionKeyLen)
	s, err := SAS(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 8 {
		t.Fatalf("SAS = %q, want 8 digits", s)
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("SAS %q is not all digits", s)
		}
	}
	// Deterministic in the key, and different keys give different SAS.
	s2, _ := SAS(key)
	if s != s2 {
		t.Error("SAS must be deterministic for a given key")
	}
	other, _ := SAS(bytes.Repeat([]byte{0x43}, SessionKeyLen))
	if s == other {
		t.Error("different keys should (almost surely) give different SAS")
	}
}
