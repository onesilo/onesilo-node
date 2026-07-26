// Package memory implements the memory capability: silo remember/recall
// backed by SQLite (FTS5 keyword search + brute-force vector search over
// Ollama embeddings when compute is available).
//
// ARCHITECTURE RULE (see CONTRIBUTING.md): this package must never import
// internal/lanserve or read pairing.key. The pairing key is a LAN transport
// secret; who may call the memory API is decided by the control plane /
// node key, and content confidentiality at rest is this package's Cipher.
package memory

// Cipher encrypts memory content at rest, keyed per silo. Production uses
// aeadCipher (AES-256-GCM, see cipher_aead.go); the swap is schema-free
// because the FTS index is always fed plaintext explicitly at write time
// and never derives text from the stored blob, so the blob can be
// ciphertext with no plaintext leaking through the index.
type Cipher interface {
	// Seal encrypts plaintext for storage under the given silo.
	Seal(siloID string, pt []byte) ([]byte, error)
	// Open decrypts a stored blob for the given silo.
	Open(siloID string, ct []byte) ([]byte, error)
}

// PlaintextCipher is a no-op cipher used only in tests that don't exercise
// at-rest crypto. Production always runs aeadCipher (wired in
// Capability.Start); content stored through this passthrough is NOT
// encrypted, so it must never be the production cipher.
type PlaintextCipher struct{}

// Seal implements Cipher.
func (PlaintextCipher) Seal(_ string, pt []byte) ([]byte, error) { return pt, nil }

// Open implements Cipher.
func (PlaintextCipher) Open(_ string, ct []byte) ([]byte, error) { return ct, nil }
