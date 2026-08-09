package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The OAuth authorization-code exchange the node performs against the
// control plane, split from the thing that drives it.
//
// There are two drivers with genuinely different shapes: `onesilo-node
// setup`, which owns the terminal and can block on its own loopback
// listener, and the admin console, which is a browser talking to an HTTP
// server that must stay responsive and cannot block on anything. What they
// share is every security-relevant step — discovery pinning, dynamic
// registration, PKCE, the code exchange — so those live here once. A second
// copy of this in the admin API would be a second place for the origin
// pinning below to be got wrong.

// FlowTimeout bounds how long an authorization may stay pending. It is the
// human's window to finish signing in, so it is generous.
const FlowTimeout = 10 * time.Minute

// ErrUserNotFound means the control plane rejected the exchange in a way
// that reads as "this person has no account", so a caller can point at
// account creation rather than at a failure.
var ErrUserNotFound = errors.New("user not found")

// OAuthMetadata is the subset of RFC 8414 discovery the node uses.
type OAuthMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

// Flow is one in-progress authorization. Everything in it is single-use:
// once Exchange succeeds the verifier and state are spent, and a Flow must
// not be reused for a second authorization.
type Flow struct {
	Meta        OAuthMetadata
	ClientID    string
	RedirectURI string
	// State is the CSRF token echoed back on the redirect. The admin
	// console's callback cannot carry the admin bearer token — a redirect
	// from the control plane is a plain browser navigation — so this is
	// what proves the callback belongs to a flow this node started.
	State string
	// Verifier is the PKCE secret. It never leaves the node except in the
	// token exchange, which is why the token endpoint is origin-pinned at
	// discovery time.
	Verifier  string
	Challenge string
	StartedAt time.Time
}

// BeginFlow performs discovery and dynamic client registration, then mints
// PKCE and state. It does not contact the browser or wait for anything: the
// caller decides how the human reaches AuthorizationURL.
//
// redirectURI is registered with the control plane as this client's only
// callback, so it must be the exact URI the caller will serve.
func BeginFlow(ctx context.Context, client *http.Client, controlPlaneURL, deviceName, redirectURI string) (*Flow, error) {
	base := strings.TrimRight(controlPlaneURL, "/")
	meta, err := DiscoverOAuth(ctx, client, base)
	if err != nil {
		return nil, fmt.Errorf("the control plane at %s does not offer OAuth sign-in: %w", base, err)
	}
	clientID, err := registerClient(ctx, client, meta.RegistrationEndpoint, deviceName, redirectURI)
	if err != nil {
		return nil, fmt.Errorf("registering this node with One Silo: %w", err)
	}
	verifier, challenge, err := pkcePair()
	if err != nil {
		return nil, err
	}
	state, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	return &Flow{
		Meta:        meta,
		ClientID:    clientID,
		RedirectURI: redirectURI,
		State:       state,
		Verifier:    verifier,
		Challenge:   challenge,
		StartedAt:   time.Now(),
	}, nil
}

// AuthorizationURL is where the human signs in.
func (f *Flow) AuthorizationURL() string {
	return f.Meta.AuthorizationEndpoint + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {f.ClientID},
		"redirect_uri":          {f.RedirectURI},
		"state":                 {f.State},
		"code_challenge":        {f.Challenge},
		"code_challenge_method": {"S256"},
	}.Encode()
}

// Expired reports whether the human took too long.
func (f *Flow) Expired(now time.Time) bool {
	return now.Sub(f.StartedAt) > FlowTimeout
}

// Exchange trades the authorization code for a credential. It does not save
// it; persisting is the caller's decision.
func (f *Flow) Exchange(ctx context.Context, client *http.Client, code string) (OAuthCredential, error) {
	return exchangeCode(ctx, client, f.Meta.TokenEndpoint, f.ClientID, code, f.Verifier, f.RedirectURI)
}

// DiscoverOAuth fetches and validates the RFC 8414 document.
func DiscoverOAuth(ctx context.Context, client *http.Client, base string) (OAuthMetadata, error) {
	var meta OAuthMetadata
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return meta, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return meta, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return meta, fmt.Errorf("discovery returned %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return meta, fmt.Errorf("parsing discovery document: %w", err)
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" || meta.RegistrationEndpoint == "" {
		return meta, errors.New("discovery document is missing required endpoints")
	}
	// Pin the discovered endpoints to the issuer's origin. Without this, a
	// hostile or MITM'd discovery document could point token_endpoint at an
	// attacker host — the node would then POST its PKCE verifier and, on
	// every future refresh, its refresh token there. The token_endpoint is
	// persisted into oauth.json, so a one-time bad discovery would leak
	// credentials indefinitely (RFC 8414 §3.3).
	for name, ep := range map[string]string{
		"authorization_endpoint": meta.AuthorizationEndpoint,
		"token_endpoint":         meta.TokenEndpoint,
		"registration_endpoint":  meta.RegistrationEndpoint,
	} {
		if err := SameSecureOrigin(base, ep); err != nil {
			return meta, fmt.Errorf("discovery %s is untrusted: %w", name, err)
		}
	}
	return meta, nil
}

// SameSecureOrigin verifies endpoint shares issuer's scheme+host+port and,
// for non-loopback issuers, is https. Loopback http is tolerated for dev.
//
// Ports are compared by their effective value, not their spelling:
// https://api.onesilo.com:443 IS https://api.onesilo.com, and RFC 8414
// metadata may legally write the default port out. Comparing the raw Host
// strings rejected that — a check meant to catch attackers was instead
// primed to break sign-in against a legitimate but explicit discovery
// document. Hostnames compare case-insensitively for the same reason: DNS
// is case-insensitive, and API.ONESILO.COM is not another host.
func SameSecureOrigin(issuer, endpoint string) error {
	iu, err := url.Parse(issuer)
	if err != nil {
		return fmt.Errorf("parsing issuer: %w", err)
	}
	eu, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("parsing endpoint: %w", err)
	}
	if eu.Scheme != iu.Scheme ||
		!strings.EqualFold(eu.Hostname(), iu.Hostname()) ||
		effectivePort(eu) != effectivePort(iu) {
		return fmt.Errorf("%s is not on the issuer's origin %s://%s", endpoint, iu.Scheme, iu.Host)
	}
	if eu.Scheme != "https" && !isLoopbackHost(eu.Hostname()) {
		return fmt.Errorf("%s is not https", endpoint)
	}
	return nil
}

// effectivePort is the port a URL actually connects to: the explicit one,
// or the scheme default when omitted.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func registerClient(ctx context.Context, client *http.Client, endpoint, deviceName, redirectURI string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"client_name":                "Silo Node (" + deviceName + ")",
		"client_uri":                 "https://github.com/onesilo/onesilo-node",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("registration returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil || reg.ClientID == "" {
		return "", errors.New("registration response is missing client_id")
	}
	return reg.ClientID, nil
}

func exchangeCode(ctx context.Context, client *http.Client, tokenEndpoint, clientID, code, verifier, redirectURI string) (OAuthCredential, error) {
	var cred OAuthCredential
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return cred, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return cred, err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
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
		return cred, ClassifySignInError(errors.New(msg))
	}
	cred = OAuthCredential{
		TokenEndpoint: tokenEndpoint,
		ClientID:      clientID,
		AccessToken:   tok.AccessToken,
		RefreshToken:  tok.RefreshToken,
	}
	if tok.ExpiresIn > 0 {
		cred.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return cred, nil
}

// ClassifySignInError maps control-plane rejections that read as "this
// person has no account" onto ErrUserNotFound, so a caller can offer account
// creation instead of reporting a bare failure.
func ClassifySignInError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	// access_denied and invalid_grant are in this list deliberately. They
	// are not literally "no account", but they are overwhelmingly what the
	// control plane returns when someone signs in with an identity that has
	// no One Silo account behind it — and "access denied" as the whole
	// explanation sends people looking for a permissions problem that isn't
	// there.
	for _, marker := range []string{
		"user not found", "no such user", "unknown user",
		"account not found", "access_denied", "invalid_grant",
	} {
		if strings.Contains(msg, marker) {
			return fmt.Errorf("%w: %s", ErrUserNotFound, err.Error())
		}
	}
	return err
}

func pkcePair() (verifier, challenge string, err error) {
	verifier, err = randomToken(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomToken(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
