package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// OAuthCredentialFile is where the sign-in credential lives under data_dir.
// Written (0600) by the `onesilo-node setup` sign-in step; consumed by
// OAuthTokenSource when control_plane.auth_mode is "oauth".
const OAuthCredentialFile = "oauth.json"

// ErrNotSignedIn means auth_mode is "oauth" but no credential is stored.
var ErrNotSignedIn = errors.New("not signed in to One Silo — run `onesilo-node setup` to sign in")

// OAuthCredential is the persisted result of the setup sign-in: the node's
// own OAuth grant against the control plane, refresh_token included. It is
// this node's identity to One Silo — the same shape of credential the Silo
// iOS app holds — and shows up as a revocable connection in the owner's
// dashboard.
type OAuthCredential struct {
	TokenEndpoint string    `json:"token_endpoint"`
	ClientID      string    `json:"client_id"`
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
}

// LoadOAuthCredential reads <dataDir>/oauth.json.
func LoadOAuthCredential(dataDir string) (OAuthCredential, error) {
	var cred OAuthCredential
	raw, err := os.ReadFile(filepath.Join(dataDir, OAuthCredentialFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cred, ErrNotSignedIn
		}
		return cred, fmt.Errorf("reading oauth credential: %w", err)
	}
	if err := json.Unmarshal(raw, &cred); err != nil {
		return cred, fmt.Errorf("parsing oauth credential: %w", err)
	}
	if cred.AccessToken == "" || cred.TokenEndpoint == "" {
		return cred, ErrNotSignedIn
	}
	return cred, nil
}

// SaveOAuthCredential persists the credential (0600).
func SaveOAuthCredential(dataDir string, cred OAuthCredential) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	raw, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, OAuthCredentialFile)
	// Atomic write (temp + rename): a crash mid-refresh must never leave a
	// truncated credential that locks the node out of its grant.
	tmp, err := os.CreateTemp(dataDir, ".oauth-*")
	if err != nil {
		return fmt.Errorf("writing oauth credential: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("writing oauth credential: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("writing oauth credential: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing oauth credential: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("writing oauth credential: %w", err)
	}
	return nil
}

// expirySkew refreshes tokens a little before they actually expire.
const expirySkew = 60 * time.Second

// OAuthTokenSource yields the stored access token, refreshing it through
// the token endpoint (refresh_token grant) when expired and persisting any
// rotated refresh token. Single-flight: concurrent callers share one
// refresh.
type OAuthTokenSource struct {
	dataDir func() (string, error)
	http    *http.Client
	now     func() time.Time

	mu     sync.Mutex
	loaded bool
	cred   OAuthCredential
}

// NewOAuthTokenSource builds a source over the live data dir (a getter so
// admin-API config changes apply without rebuilding).
func NewOAuthTokenSource(dataDir func() (string, error)) *OAuthTokenSource {
	return &OAuthTokenSource{
		dataDir: dataDir,
		http:    &http.Client{Timeout: 15 * time.Second},
		now:     time.Now,
	}
}

// Invalidate drops the cached credential so the next Token() re-reads the
// file (used after a fresh sign-in).
func (s *OAuthTokenSource) Invalidate() {
	s.mu.Lock()
	s.loaded = false
	s.mu.Unlock()
}

// Token implements TokenSource.
func (s *OAuthTokenSource) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir, err := s.dataDir()
	if err != nil {
		return "", err
	}
	if !s.loaded {
		cred, err := LoadOAuthCredential(dir)
		if err != nil {
			return "", err
		}
		s.cred = cred
		s.loaded = true
	}

	if s.cred.ExpiresAt.IsZero() || s.now().Add(expirySkew).Before(s.cred.ExpiresAt) {
		return s.cred.AccessToken, nil
	}
	if s.cred.RefreshToken == "" {
		return "", fmt.Errorf("%w (access token expired and no refresh token stored)", ErrNotSignedIn)
	}

	refreshed, err := refreshGrant(s.http, s.cred)
	if err != nil {
		return "", fmt.Errorf("refreshing One Silo sign-in: %w", err)
	}
	s.cred = refreshed
	if err := SaveOAuthCredential(dir, s.cred); err != nil {
		return "", err
	}
	return s.cred.AccessToken, nil
}

// tokenResponse is the OAuth token endpoint response subset we use.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// refreshGrant exchanges the refresh token for a fresh access token,
// keeping the old refresh token when the server doesn't rotate it.
func refreshGrant(client *http.Client, cred OAuthCredential) (OAuthCredential, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {cred.RefreshToken},
		"client_id":     {cred.ClientID},
	}
	resp, err := client.Post(cred.TokenEndpoint, "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		return cred, err
	}
	defer resp.Body.Close()
	var tok tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return cred, fmt.Errorf("parsing token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		msg := tok.Error
		if tok.ErrorDesc != "" {
			msg += ": " + tok.ErrorDesc
		}
		if msg == "" {
			msg = resp.Status
		}
		return cred, errors.New(msg)
	}
	next := cred
	next.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		next.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		next.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	} else {
		next.ExpiresAt = time.Time{}
	}
	return next, nil
}
