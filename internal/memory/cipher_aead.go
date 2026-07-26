package memory

import (
	"crypto/aes"
	"crypto/cipher"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// memoryKeyFile holds the per-node memory data key (0600).
const memoryKeyFile = "memory.key"

// aeadCipher encrypts memory content at rest with AES-256-GCM, keyed by a
// per-node data key at <data_dir>/memory.key. Every record gets a fresh
// random nonce (stored prepended to the ciphertext), and the silo id is
// bound in as additional authenticated data — so a stored blob cannot be
// decrypted under, or relocated to, a different silo, and any tampering
// with the ciphertext fails the GCM tag.
type aeadCipher struct {
	aead cipher.AEAD
}

// NewAEADCipher loads (or creates, 0600) the per-node memory key and
// returns an AES-256-GCM cipher over it. This is the production Cipher;
// PlaintextCipher remains for tests that don't exercise at-rest crypto.
func NewAEADCipher(dataDir string) (Cipher, error) {
	key, err := loadOrCreateMemoryKey(dataDir)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("memory cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("memory cipher: %w", err)
	}
	return &aeadCipher{aead: aead}, nil
}

// Seal implements Cipher: nonce || AES-256-GCM(pt) with siloID as AAD.
func (c *aeadCipher) Seal(siloID string, pt []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := crand.Read(nonce); err != nil {
		return nil, fmt.Errorf("sealing memory: %w", err)
	}
	// Seal appends the ciphertext+tag to nonce, so the result is
	// nonce||ciphertext and Open can recover the nonce by slicing.
	return c.aead.Seal(nonce, nonce, pt, []byte(siloID)), nil
}

// Open implements Cipher: verifies siloID as AAD and the GCM tag.
func (c *aeadCipher) Open(siloID string, ct []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(ct) < ns {
		return nil, fmt.Errorf("sealed memory blob is too short")
	}
	nonce, body := ct[:ns], ct[ns:]
	pt, err := c.aead.Open(nil, nonce, body, []byte(siloID))
	if err != nil {
		return nil, fmt.Errorf("decrypting memory (wrong silo or corrupt/tampered data): %w", err)
	}
	return pt, nil
}

func loadOrCreateMemoryKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, memoryKeyFile)
	raw, err := os.ReadFile(path)
	if err == nil {
		key, decErr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil || len(key) != 32 {
			return nil, fmt.Errorf("memory key at %s is malformed (want 64 hex chars)", path)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading memory key: %w", err)
	}
	key := make([]byte, 32)
	if _, err := crand.Read(key); err != nil {
		return nil, fmt.Errorf("generating memory key: %w", err)
	}
	if err := writeFileAtomic(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, fmt.Errorf("writing memory key: %w", err)
	}
	return key, nil
}

// writeFileAtomic writes data to a temp file in the same directory and
// renames it into place, so a crash mid-write can never leave a truncated
// secret. The temp file is created with perm from the start (never wider).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
