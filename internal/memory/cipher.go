// Package memory implements the memory capability: silo remember/recall
// backed by SQLite (FTS5 keyword search + brute-force vector search over
// Ollama embeddings when compute is available).
//
// ARCHITECTURE RULE (see CONTRIBUTING.md): this package must never import
// internal/lanserve or read pairing.key. The pairing key is a LAN transport
// secret; who may call the memory API is decided by the control plane /
// node key, and content confidentiality at rest is this package's Cipher.
package memory

// Cipher encrypts memory content at rest, keyed per silo. v1 ships the
// plaintext passthrough; a real cipher can be swapped in without schema
// changes because the FTS index is always fed plaintext explicitly at
// write time (it never derives text from the stored blob).
type Cipher interface {
	// Seal encrypts plaintext for storage under the given silo.
	Seal(siloID string, pt []byte) ([]byte, error)
	// Open decrypts a stored blob for the given silo.
	Open(siloID string, ct []byte) ([]byte, error)
}

// PlaintextCipher is the v1 no-op cipher: content is stored as-is.
type PlaintextCipher struct{}

// Seal implements Cipher.
func (PlaintextCipher) Seal(_ string, pt []byte) ([]byte, error) { return pt, nil }

// Open implements Cipher.
func (PlaintextCipher) Open(_ string, ct []byte) ([]byte, error) { return ct, nil }
