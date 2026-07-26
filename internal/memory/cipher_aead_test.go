package memory

import (
	"bytes"
	"os"
	"testing"
)

func TestAEADCipherRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c, err := NewAEADCipher(dir)
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("the launch ships on Friday")
	ct, err := c.Seal("personal", pt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, pt) {
		t.Fatal("ciphertext contains plaintext — not actually encrypted")
	}
	got, err := c.Open("personal", ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round trip = %q, want %q", got, pt)
	}
}

func TestAEADCipherRejectsWrongSilo(t *testing.T) {
	dir := t.TempDir()
	c, _ := NewAEADCipher(dir)
	ct, _ := c.Seal("silo-a", []byte("secret"))
	// The silo id is bound as AAD, so opening under another silo must fail.
	if _, err := c.Open("silo-b", ct); err == nil {
		t.Fatal("decrypted under the wrong silo — AAD binding is broken")
	}
}

func TestAEADCipherRejectsTamper(t *testing.T) {
	dir := t.TempDir()
	c, _ := NewAEADCipher(dir)
	ct, _ := c.Seal("personal", []byte("secret"))
	ct[len(ct)-1] ^= 0xff // flip a tag byte
	if _, err := c.Open("personal", ct); err == nil {
		t.Fatal("opened tampered ciphertext — GCM tag not enforced")
	}
}

func TestAEADCipherPersistsKey(t *testing.T) {
	dir := t.TempDir()
	c1, _ := NewAEADCipher(dir)
	ct, _ := c1.Seal("personal", []byte("durable"))

	// A second cipher over the same data dir must reuse the stored key and
	// decrypt what the first one sealed.
	c2, err := NewAEADCipher(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c2.Open("personal", ct)
	if err != nil {
		t.Fatalf("second cipher could not open first's ciphertext: %v", err)
	}
	if string(got) != "durable" {
		t.Fatalf("got %q", got)
	}

	// The key file must be 0600.
	info, err := os.Stat(dir + "/" + memoryKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("memory.key perm = %o, want 600", perm)
	}
}
