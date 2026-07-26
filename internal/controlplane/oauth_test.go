package controlplane

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOAuthCredentialRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cred := OAuthCredential{
		TokenEndpoint: "https://api.example.com/oauth/token",
		ClientID:      "client-1",
		AccessToken:   "at-1",
		RefreshToken:  "rt-1",
		ExpiresAt:     time.Now().Add(time.Hour).UTC(),
	}
	if err := SaveOAuthCredential(dir, cred); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, OAuthCredentialFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected 0600, got %o", perm)
	}
	got, err := LoadOAuthCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "at-1" || got.RefreshToken != "rt-1" || got.ClientID != "client-1" {
		t.Fatalf("unexpected credential %+v", got)
	}
}

func TestLoadOAuthCredentialMissingIsNotSignedIn(t *testing.T) {
	if _, err := LoadOAuthCredential(t.TempDir()); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("expected ErrNotSignedIn, got %v", err)
	}
}

func TestOAuthTokenSourceReturnsValidToken(t *testing.T) {
	dir := t.TempDir()
	if err := SaveOAuthCredential(dir, OAuthCredential{
		TokenEndpoint: "https://unused.example.com",
		ClientID:      "c",
		AccessToken:   "fresh",
		RefreshToken:  "rt",
		ExpiresAt:     time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	src := NewOAuthTokenSource(func() (string, error) { return dir, nil })
	token, err := src.Token()
	if err != nil || token != "fresh" {
		t.Fatalf("expected the stored token, got %q, %v", token, err)
	}
}

func TestOAuthTokenSourceRefreshesAndPersists(t *testing.T) {
	var seenGrant, seenRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seenGrant = r.Form.Get("grant_type")
		seenRefresh = r.Form.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token": "at-2", "refresh_token": "rt-2", "expires_in": 3600}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := SaveOAuthCredential(dir, OAuthCredential{
		TokenEndpoint: srv.URL,
		ClientID:      "c",
		AccessToken:   "at-1",
		RefreshToken:  "rt-1",
		ExpiresAt:     time.Now().Add(-time.Minute), // expired
	}); err != nil {
		t.Fatal(err)
	}
	src := NewOAuthTokenSource(func() (string, error) { return dir, nil })
	token, err := src.Token()
	if err != nil {
		t.Fatal(err)
	}
	if token != "at-2" || seenGrant != "refresh_token" || seenRefresh != "rt-1" {
		t.Fatalf("unexpected refresh: token=%q grant=%q refresh=%q", token, seenGrant, seenRefresh)
	}
	// The rotated refresh token must be persisted.
	stored, err := LoadOAuthCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "at-2" || stored.RefreshToken != "rt-2" {
		t.Fatalf("rotation not persisted: %+v", stored)
	}
}

func TestOAuthTokenSourceExpiredWithoutRefreshToken(t *testing.T) {
	dir := t.TempDir()
	if err := SaveOAuthCredential(dir, OAuthCredential{
		TokenEndpoint: "https://unused.example.com",
		ClientID:      "c",
		AccessToken:   "stale",
		ExpiresAt:     time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	src := NewOAuthTokenSource(func() (string, error) { return dir, nil })
	if _, err := src.Token(); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("expected ErrNotSignedIn, got %v", err)
	}
}

func TestModalTokenSourceOAuthMode(t *testing.T) {
	dir := t.TempDir()
	if err := SaveOAuthCredential(dir, OAuthCredential{
		TokenEndpoint: "https://unused.example.com",
		ClientID:      "c",
		AccessToken:   "oauth-token",
		ExpiresAt:     time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	src := &ModalTokenSource{
		Mode:   func() string { return "oauth" },
		JWT:    &JWTStore{},
		APIKey: NewAPIKeyStoreWithLookup(func(string) (string, bool) { return "", false }),
		OAuth:  NewOAuthTokenSource(func() (string, error) { return dir, nil }),
	}
	token, err := src.Token()
	if err != nil || token != "oauth-token" {
		t.Fatalf("expected the oauth token, got %q, %v", token, err)
	}
	// Nil OAuth source fails closed instead of panicking.
	nilSrc := &ModalTokenSource{Mode: func() string { return "oauth" }, JWT: &JWTStore{}, APIKey: NewAPIKeyStoreWithLookup(func(string) (string, bool) { return "", false })}
	if _, err := nilSrc.Token(); !errors.Is(err, ErrNoToken) {
		t.Fatalf("expected ErrNoToken, got %v", err)
	}
}
