// Package controlplane registers this node with the Silo backend
// (POST/DELETE /api/v1/destinations, heartbeats) and manages the auth
// tokens those calls carry.
package controlplane

import (
	"errors"
	"os"
	"sync"
)

// ErrNoToken means no auth token is available yet: the desktop app hasn't
// pushed a JWT, or SILO_API_KEY is unset.
var ErrNoToken = errors.New("no control-plane auth token available")

// TokenSource yields the bearer token for control-plane requests.
type TokenSource interface {
	Token() (string, error)
}

// JWTStore holds a Clerk JWT pushed by the desktop app through the admin
// API (POST /v1/auth/jwt). In-memory only — JWTs are short-lived and are
// re-pushed on refresh.
type JWTStore struct {
	mu    sync.RWMutex
	token string
}

// Set replaces the stored JWT. Setting "" clears it.
func (s *JWTStore) Set(token string) {
	s.mu.Lock()
	s.token = token
	s.mu.Unlock()
}

// Token implements TokenSource.
func (s *JWTStore) Token() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.token == "" {
		return "", ErrNoToken
	}
	return s.token, nil
}

// APIKeyEnvVar is where headless deployments provide an sc_ API key.
const APIKeyEnvVar = "SILO_API_KEY"

// APIKeyStore reads an sc_ API key from the environment.
type APIKeyStore struct {
	lookup func(string) (string, bool)
}

// NewAPIKeyStore reads from the real environment.
func NewAPIKeyStore() *APIKeyStore {
	return &APIKeyStore{lookup: os.LookupEnv}
}

// NewAPIKeyStoreWithLookup is the test seam.
func NewAPIKeyStoreWithLookup(lookup func(string) (string, bool)) *APIKeyStore {
	return &APIKeyStore{lookup: lookup}
}

// Token implements TokenSource.
func (s *APIKeyStore) Token() (string, error) {
	if v, ok := s.lookup(APIKeyEnvVar); ok && v != "" {
		return v, nil
	}
	return "", ErrNoToken
}

// ModalTokenSource picks between JWT, API-key, and OAuth auth based on the
// live config value, so a PUT /v1/config auth_mode change applies
// immediately.
type ModalTokenSource struct {
	Mode   func() string // returns a config.AuthMode* value
	JWT    TokenSource
	APIKey TokenSource
	OAuth  TokenSource // may be nil (falls back to ErrNoToken)
}

// Token implements TokenSource.
func (m *ModalTokenSource) Token() (string, error) {
	switch m.Mode() {
	case "api_key":
		return m.APIKey.Token()
	case "oauth":
		if m.OAuth == nil {
			return "", ErrNoToken
		}
		return m.OAuth.Token()
	default:
		return m.JWT.Token()
	}
}
