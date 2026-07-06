package lanserve

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// Golden-vector parameters shared with the iOS/macOS CryptoKit
// implementation: AES-256-GCM combined format (nonce12 || ct || tag16).
const (
	goldenKeyHex   = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	goldenNonceHex = "0102030405060708090a0b0c"
	goldenPlain    = "hello silo"
	// goldenCombinedB64 is the combined ciphertext for the parameters above.
	// Pinned so an accidental format change (nonce placement, tag size, AAD)
	// fails loudly instead of silently breaking iOS interop.
	goldenCombinedB64 = "AQIDBAUGBwgJCgsMbY82uYO0g+8gzdqORRQ3Uq6oY97qKBlISOI="
)

func goldenKey(t *testing.T) []byte {
	t.Helper()
	key, err := hex.DecodeString(goldenKeyHex)
	if err != nil {
		t.Fatalf("decoding key: %v", err)
	}
	return key
}

func TestGoldenVectorDecrypt(t *testing.T) {
	key := goldenKey(t)
	combined, err := base64.StdEncoding.DecodeString(goldenCombinedB64)
	if err != nil {
		t.Fatalf("decoding fixture: %v", err)
	}

	// Structural format assumptions: combined = nonce(12) || ct || tag(16).
	if want := nonceSize + len(goldenPlain) + tagSize; len(combined) != want {
		t.Fatalf("combined length = %d, want %d (12 + len(pt) + 16)", len(combined), want)
	}
	nonce, err := hex.DecodeString(goldenNonceHex)
	if err != nil {
		t.Fatalf("decoding nonce: %v", err)
	}
	if !bytes.Equal(combined[:nonceSize], nonce) {
		t.Fatalf("combined does not start with the nonce: %x", combined[:nonceSize])
	}

	// The fixture must equal what Go itself computes with the same
	// deterministic nonce…
	recomputed, err := sealWithNonce(key, nonce, []byte(goldenPlain))
	if err != nil {
		t.Fatalf("sealWithNonce: %v", err)
	}
	if got := base64.StdEncoding.EncodeToString(recomputed); got != goldenCombinedB64 {
		t.Fatalf("recomputed combined = %s, want %s", got, goldenCombinedB64)
	}

	// …and decrypt back to the plaintext.
	pt, err := Open(key, combined)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(pt) != goldenPlain {
		t.Fatalf("decrypted %q, want %q", pt, goldenPlain)
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	key := goldenKey(t)
	for _, plaintext := range []string{"", "x", goldenPlain, string(bytes.Repeat([]byte("payload "), 1000))} {
		combined, err := Seal(key, []byte(plaintext))
		if err != nil {
			t.Fatalf("Seal(%d bytes): %v", len(plaintext), err)
		}
		if want := nonceSize + len(plaintext) + tagSize; len(combined) != want {
			t.Fatalf("combined length = %d, want %d", len(combined), want)
		}
		pt, err := Open(key, combined)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if string(pt) != plaintext {
			t.Fatalf("round trip mismatch: got %d bytes, want %d", len(pt), len(plaintext))
		}
	}

	// Nonces must be unique per Seal.
	a, _ := Seal(key, []byte("same"))
	b, _ := Seal(key, []byte("same"))
	if bytes.Equal(a[:nonceSize], b[:nonceSize]) {
		t.Fatal("two Seals produced the same nonce")
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	key := goldenKey(t)
	combined, err := Seal(key, []byte(goldenPlain))
	if err != nil {
		t.Fatal(err)
	}

	// Flip one bit at every region: nonce, ciphertext, tag.
	for _, pos := range []int{0, nonceSize, nonceSize + 3, len(combined) - 1} {
		tampered := bytes.Clone(combined)
		tampered[pos] ^= 0x01
		if _, err := Open(key, tampered); err == nil {
			t.Fatalf("Open accepted ciphertext with bit flipped at %d", pos)
		}
	}

	// Truncated input must error, not panic.
	if _, err := Open(key, combined[:nonceSize+tagSize-1]); err == nil {
		t.Fatal("Open accepted truncated input")
	}

	// Wrong key must fail.
	otherKey := bytes.Clone(key)
	otherKey[0] ^= 0xff
	if _, err := Open(otherKey, combined); err == nil {
		t.Fatal("Open accepted ciphertext under the wrong key")
	}
}

func TestSealRejectsBadKeySize(t *testing.T) {
	if _, err := Seal([]byte("short"), []byte("pt")); err == nil {
		t.Fatal("Seal accepted a non-32-byte key")
	}
	if _, err := Open([]byte("short"), make([]byte, 64)); err == nil {
		t.Fatal("Open accepted a non-32-byte key")
	}
}

func TestLoadPairingKey(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadPairingKey(dir); err != ErrNoKey {
		t.Fatalf("missing file: got %v, want ErrNoKey", err)
	}

	path := filepath.Join(dir, "pairing.key")
	if err := os.WriteFile(path, []byte(goldenKeyHex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := LoadPairingKey(dir)
	if err != nil {
		t.Fatalf("LoadPairingKey: %v", err)
	}
	if !bytes.Equal(key, goldenKey(t)) {
		t.Fatal("loaded key mismatch")
	}

	if err := os.WriteFile(path, []byte("not-hex"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPairingKey(dir); err == nil {
		t.Fatal("LoadPairingKey accepted invalid hex")
	}

	src := FileKeySource(func() (string, error) { return dir, nil })
	if src() != nil {
		t.Fatal("FileKeySource returned a key for an invalid file")
	}
	os.WriteFile(path, []byte(goldenKeyHex), 0o600)
	if !bytes.Equal(src(), goldenKey(t)) {
		t.Fatal("FileKeySource did not pick up the new key")
	}
}
