// Package openaiapi is the OpenAI-compatible inference surface: it mounts
// /v1/chat/completions, /v1/completions, /v1/models, and /v1/embeddings on
// the LAN server (and therefore behind the tunnel) and reverse-proxies them
// to the local Ollama server's native OpenAI-compatible API.
//
// This surface is deliberately separate from the node's end-to-end
// encrypted Silo protocol. Third-party OpenAI clients (IDEs, SDKs) speak it
// unmodified, authenticated by bearer API keys minted through the admin
// API; content is plaintext at the protocol level, so operators choosing a
// managed tunnel accept that the tunnel edge can see this traffic. The E2E
// surface's guarantees are unchanged — nothing here can touch memory or
// pairing.
package openaiapi

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/onesilo/onesilo-node/internal/fsutil"
)

// keyPrefix marks node inference keys; the raw secret follows it.
const keyPrefix = "silo_sk_"

// keysFile is the store's filename inside the node data dir.
const keysFile = "openai_keys.json"

// ErrKeyNotFound is returned by Revoke for an unknown key id.
var ErrKeyNotFound = errors.New("no such API key")

// Key is one minted API key's metadata. The secret itself is never stored —
// only its SHA-256 — so a stolen store file cannot authenticate.
type Key struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	SHA256    string    `json:"sha256"`
	Last4     string    `json:"last4"`
	CreatedAt time.Time `json:"created_at"`
}

// KeyStore holds the minted keys, persisted as JSON in the data dir.
type KeyStore struct {
	mu   sync.RWMutex
	path string
	keys []Key
}

// LoadKeyStore reads the store from dataDir, treating a missing file as
// empty. Corrupt stores are an error — failing open would silently drop
// keys, failing closed refuses new mints until the operator intervenes.
func LoadKeyStore(dataDir string) (*KeyStore, error) {
	s := &KeyStore{path: filepath.Join(dataDir, keysFile)}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading API key store: %w", err)
	}
	if err := json.Unmarshal(raw, &s.keys); err != nil {
		return nil, fmt.Errorf("parsing API key store %s: %w", s.path, err)
	}
	return s, nil
}

// Mint creates a key, persists its hash, and returns the plaintext — the
// only time it is ever available.
func (s *KeyStore) Mint(name string) (plaintext string, key Key, err error) {
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return "", Key{}, fmt.Errorf("generating API key: %w", err)
	}
	plaintext = keyPrefix + hex.EncodeToString(secret)

	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", Key{}, fmt.Errorf("generating API key id: %w", err)
	}

	sum := sha256.Sum256([]byte(plaintext))
	key = Key{
		ID:        "key_" + hex.EncodeToString(idBytes),
		Name:      strings.TrimSpace(name),
		SHA256:    hex.EncodeToString(sum[:]),
		Last4:     plaintext[len(plaintext)-4:],
		CreatedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, key)
	if err := s.persistLocked(); err != nil {
		s.keys = s.keys[:len(s.keys)-1]
		return "", Key{}, err
	}
	return plaintext, key, nil
}

// Revoke deletes the key with the given id.
func (s *KeyStore) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.keys {
		if k.ID == id {
			// Copy rather than splice so the original slice survives a
			// failed persist — memory and disk must not disagree.
			remaining := make([]Key, 0, len(s.keys)-1)
			remaining = append(remaining, s.keys[:i]...)
			remaining = append(remaining, s.keys[i+1:]...)
			prev := s.keys
			s.keys = remaining
			if err := s.persistLocked(); err != nil {
				s.keys = prev
				return err
			}
			return nil
		}
	}
	return ErrKeyNotFound
}

// List returns the keys sorted oldest-first. Metadata only — hashes are
// included in Key but callers shaping API responses should drop them.
func (s *KeyStore) List() []Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Key, len(s.keys))
	copy(out, s.keys)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Count returns the number of minted keys.
func (s *KeyStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// Verify reports whether the presented secret matches a minted key. The
// comparison is constant-time on the hash; the store never sees plaintext
// except transiently here and in Mint.
func (s *KeyStore) Verify(presented string) bool {
	if !strings.HasPrefix(presented, keyPrefix) {
		return false
	}
	sum := sha256.Sum256([]byte(presented))
	presentedHex := []byte(hex.EncodeToString(sum[:]))

	s.mu.RLock()
	defer s.mu.RUnlock()
	// Compare against every stored hash and decide only after the loop,
	// so verification time doesn't reveal key position or whether a match
	// occurred.
	match := 0
	for _, k := range s.keys {
		match |= subtle.ConstantTimeCompare(presentedHex, []byte(k.SHA256))
	}
	return match == 1
}

func (s *KeyStore) persistLocked() error {
	raw, err := json.MarshalIndent(s.keys, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding API key store: %w", err)
	}
	if err := fsutil.WriteFileAtomic(s.path, raw, 0o600); err != nil {
		return fmt.Errorf("writing API key store: %w", err)
	}
	return nil
}
