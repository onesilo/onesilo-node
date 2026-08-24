package openaiapi

import "testing"

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
