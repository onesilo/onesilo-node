package openaiapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMintForControlPlaneNamesTheKeyAndVerifies(t *testing.T) {
	s, err := LoadKeyStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadKeyStore: %v", err)
	}

	plaintext, superseded, err := s.MintForControlPlane()
	if err != nil {
		t.Fatalf("MintForControlPlane: %v", err)
	}
	if len(superseded) != 0 {
		t.Errorf("first mint superseded %v, want nothing", superseded)
	}
	if !s.Verify(plaintext) {
		t.Error("the minted key does not authenticate against the surface")
	}
	keys := s.List()
	if len(keys) != 1 || keys[0].Name != ControlPlaneKeyName {
		t.Errorf("stored keys = %+v, want one named %q", keys, ControlPlaneKeyName)
	}
}

func TestASecondMintReportsTheFirstAsSupersededAndLeavesItValid(t *testing.T) {
	// Both must work until the caller commits: the control plane still
	// holds the old one while the registration carrying the new one is in
	// flight, and that flight can fail.
	s, _ := LoadKeyStore(t.TempDir())
	first, _, _ := s.MintForControlPlane()

	second, superseded, err := s.MintForControlPlane()
	if err != nil {
		t.Fatalf("second MintForControlPlane: %v", err)
	}
	if len(superseded) != 1 {
		t.Fatalf("superseded = %v, want exactly the first key", superseded)
	}
	if !s.Verify(first) {
		t.Error("the superseded key stopped working before it was revoked")
	}
	if !s.Verify(second) {
		t.Error("the new key does not authenticate")
	}

	if err := s.RevokeAll(superseded); err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	if s.Verify(first) {
		t.Error("the superseded key still works after being revoked")
	}
	if !s.Verify(second) {
		t.Error("revoking the old key took the new one with it")
	}
}

func TestMintForControlPlaneLeavesAPersonsKeysAlone(t *testing.T) {
	// Someone's IDE key must not be collateral damage in a rotation it has
	// nothing to do with.
	s, _ := LoadKeyStore(t.TempDir())
	mine, _, err := s.Mint("my laptop")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, _, _ = s.MintForControlPlane()
	_, superseded, _ := s.MintForControlPlane()

	if err := s.RevokeAll(superseded); err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	if !s.Verify(mine) {
		t.Error("rotating the control-plane key revoked a person's key")
	}
}

func TestRevokeAllToleratesKeysAlreadyGone(t *testing.T) {
	// The operator can revoke through the admin API at any moment, so a
	// commit landing afterwards must not turn that into an error.
	s, _ := LoadKeyStore(t.TempDir())
	if err := s.RevokeAll([]string{"key_deadbeefdeadbeef"}); err != nil {
		t.Errorf("RevokeAll on an unknown id = %v, want nil", err)
	}
}

func TestMintedControlPlaneKeysSurviveAReload(t *testing.T) {
	// The node restarts far more often than it re-registers; a key the
	// control plane holds has to still verify afterwards.
	dir := t.TempDir()
	s, _ := LoadKeyStore(dir)
	plaintext, _, _ := s.MintForControlPlane()

	reloaded, err := LoadKeyStore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.Verify(plaintext) {
		t.Error("the key did not survive a restart")
	}
}

func TestAPersonalKeyNamedControlPlaneIsNeverRotatedAway(t *testing.T) {
	// The admin API mints under whatever name the operator types
	// (Node.MintOpenAIKey passes it straight through), so the name cannot
	// be evidence of who a key belongs to. Rotating on the name would let
	// someone destroy their own working key by choosing an unlucky one.
	s, _ := LoadKeyStore(t.TempDir())
	decoy, _, err := s.Mint(ControlPlaneKeyName)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, superseded, err := s.MintForControlPlane()
	if err != nil {
		t.Fatalf("MintForControlPlane: %v", err)
	}
	if len(superseded) != 0 {
		t.Fatalf("superseded %v — a person's key was claimed as the control plane's", superseded)
	}

	if err := s.RevokeAll(superseded); err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	if !s.Verify(decoy) {
		t.Error("the operator's own key was revoked because of its name")
	}
}

func TestOnlyMintForControlPlaneCanSetTheScope(t *testing.T) {
	s, _ := LoadKeyStore(t.TempDir())
	if _, k, _ := s.Mint(ControlPlaneKeyName); k.Scope != "" {
		t.Errorf("a key minted through the public API carries scope %q", k.Scope)
	}
	if _, _, err := s.MintForControlPlane(); err != nil {
		t.Fatalf("MintForControlPlane: %v", err)
	}
	scoped := 0
	for _, k := range s.List() {
		if k.Scope == ControlPlaneKeyScope {
			scoped++
		}
	}
	if scoped != 1 {
		t.Errorf("%d keys carry the control-plane scope, want exactly 1", scoped)
	}
}

func TestAKeyStoredBeforeScopesExistedIsNeverRotatedAway(t *testing.T) {
	// Stores written before Scope existed have no such field. Reading that
	// absence as "the control plane's" would revoke a key on the first
	// rotation after an upgrade; reading it as "someone's" costs nothing.
	dir := t.TempDir()
	legacy := filepath.Join(dir, keysFile)
	sum := sha256.Sum256([]byte("silo_sk_legacy"))
	raw := fmt.Sprintf(
		`[{"id":"key_legacy0000000","name":"control-plane","sha256":%q,`+
			`"last4":"gacy","created_at":"2026-01-01T00:00:00Z"}]`,
		hex.EncodeToString(sum[:]),
	)
	if err := os.WriteFile(legacy, []byte(raw), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s, err := LoadKeyStore(dir)
	if err != nil {
		t.Fatalf("LoadKeyStore: %v", err)
	}
	_, superseded, err := s.MintForControlPlane()
	if err != nil {
		t.Fatalf("MintForControlPlane: %v", err)
	}
	if len(superseded) != 0 {
		t.Errorf("superseded %v — an unscoped legacy key was claimed", superseded)
	}
	if !s.Verify("silo_sk_legacy") {
		t.Error("the legacy key was revoked")
	}
}
