package pairing

import (
	"errors"
	"testing"
)

func TestPinStoreFirstContactThenVerify(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadPinStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.Observe("acct-1", "APPKEY-A")
	if err != nil {
		t.Fatal(err)
	}
	if status != PinFirstContact {
		t.Fatalf("first observe should be first contact, got %v", status)
	}
	if s.IsVerified("acct-1", "APPKEY-A") {
		t.Fatal("first contact must not be pre-verified")
	}
	if err := s.Verify("acct-1", "APPKEY-A"); err != nil {
		t.Fatal(err)
	}
	if !s.IsVerified("acct-1", "APPKEY-A") {
		t.Fatal("key should be verified after Verify")
	}
	// A second observe of the same verified key reports known-verified.
	if status, _ := s.Observe("acct-1", "APPKEY-A"); status != PinKnownVerified {
		t.Fatalf("re-observe should be known-verified, got %v", status)
	}
}

func TestPinStorePersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadPinStore(dir)
	if _, err := s.Observe("acct-1", "APPKEY-A"); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify("acct-1", "APPKEY-A"); err != nil {
		t.Fatal(err)
	}
	// Reload from disk: the verified pin must survive.
	s2, err := LoadPinStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.IsVerified("acct-1", "APPKEY-A") {
		t.Fatal("verified pin did not persist across reload")
	}
}

func TestPinStoreSafetyNumberChange(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadPinStore(dir)
	if _, err := s.Observe("acct-1", "APPKEY-A"); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify("acct-1", "APPKEY-A"); err != nil {
		t.Fatal(err)
	}
	// A *different* key for the same account, after a verified pin, is a hard
	// stop — the classic safety-number change.
	if _, err := s.Observe("acct-1", "APPKEY-B"); !errors.Is(err, ErrSafetyNumberChanged) {
		t.Fatalf("expected ErrSafetyNumberChanged, got %v", err)
	}
}

func TestPinStoreDifferentAccountsIndependent(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadPinStore(dir)
	if _, err := s.Observe("acct-1", "APPKEY-A"); err != nil {
		t.Fatal(err)
	}
	_ = s.Verify("acct-1", "APPKEY-A")
	// A different account's first key is a normal first contact, not a
	// safety-number change.
	if status, err := s.Observe("acct-2", "APPKEY-Z"); err != nil || status != PinFirstContact {
		t.Fatalf("cross-account observe should be a clean first contact, got %v / %v", status, err)
	}
}

func TestPinStoreUnverifiedKeyReplacedByNewFirstContact(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadPinStore(dir)
	// First contact, never verified.
	if _, err := s.Observe("acct-1", "APPKEY-A"); err != nil {
		t.Fatal(err)
	}
	// A different key while the prior one is only *unverified* is a fresh
	// first contact, not a safety-number change.
	if status, err := s.Observe("acct-1", "APPKEY-B"); err != nil || status != PinFirstContact {
		t.Fatalf("expected a clean first contact, got %v / %v", status, err)
	}
}

func TestVerifyUnknownKeyErrors(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadPinStore(dir)
	if err := s.Verify("acct-1", "NEVER-SEEN"); err == nil {
		t.Fatal("verifying a never-observed key should error")
	}
}
