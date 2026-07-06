// Package lanserve implements LAN serving: the WebSocket protocol the Silo
// iOS app speaks to a paired device on the local network, Bonjour discovery,
// and the HTTP surface (health, memory API mount) on lan.port. The wire
// format is a byte-for-byte port of the SiloMac Swift prototype
// (WebSocketServer/MessageRouter/LLMMessageHandler/ServingCrypto): plain
// JSON text frames for pre-pairing traffic, AES-256-GCM encrypted envelopes
// for everything else. See docs/protocol.md.
package lanserve

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	nonceSize = 12 // AES-GCM standard nonce, prepended to the ciphertext
	tagSize   = 16 // AES-GCM tag, appended by Seal
)

// ErrNoKey means no pairing key is stored yet.
var ErrNoKey = errors.New("no pairing key stored")

// Envelope is the encrypted wrapper both directions use once paired.
// Content is base64 of the AES-256-GCM "combined" form
// (nonce12 || ciphertext || tag16) — CryptoKit's AES.GCM.SealedBox.combined.
// UserID travels unencrypted and is echoed back verbatim on responses.
type Envelope struct {
	Content string `json:"content"`
	UserID  string `json:"user_id"`
}

// Seal encrypts plaintext with AES-256-GCM under key and returns the
// combined form: a fresh random 12-byte nonce, then ciphertext, then the
// 16-byte tag. Matches CryptoKit's SealedBox.combined byte-for-byte.
func Seal(key, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	return sealWithNonce(key, nonce, plaintext)
}

// sealWithNonce is Seal with a caller-provided nonce (tests / golden vectors).
func sealWithNonce(key, nonce, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != nonceSize {
		return nil, fmt.Errorf("nonce must be %d bytes, got %d", nonceSize, len(nonce))
	}
	// Seal appends ciphertext||tag to the nonce: combined format.
	return gcm.Seal(nonce[:len(nonce):len(nonce)], nonce, plaintext, nil), nil
}

// Open decrypts combined-format data (nonce12 || ciphertext || tag16)
// produced by Seal or by CryptoKit on the iOS side.
func Open(key, combined []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(combined) < nonceSize+tagSize {
		return nil, fmt.Errorf("combined data too short: %d bytes", len(combined))
	}
	return gcm.Open(nil, combined[:nonceSize], combined[nonceSize:], nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("pairing key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// LoadPairingKey reads <dataDir>/pairing.key (64 hex chars written by the
// admin API's POST /v1/auth/pairing-key) and returns the 32-byte key.
// Returns ErrNoKey when the file does not exist.
func LoadPairingKey(dataDir string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "pairing.key"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, fmt.Errorf("reading pairing key: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("pairing.key must contain 64 hex characters")
	}
	return key, nil
}

// FileKeySource returns a KeySource that re-reads the pairing key on every
// call, so a key pushed through the admin API applies to the next message
// without restarting the server (mirrors SiloMac's per-message
// cryptoProvider). Returns nil when no valid key is stored.
func FileKeySource(dataDir func() (string, error)) func() []byte {
	return func() []byte {
		dir, err := dataDir()
		if err != nil {
			return nil
		}
		key, err := LoadPairingKey(dir)
		if err != nil {
			return nil
		}
		return key
	}
}
