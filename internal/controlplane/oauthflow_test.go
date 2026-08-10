package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Origin pinning is the control that stops a hostile or MITM'd discovery
// document from harvesting this node's credentials: token_endpoint is
// persisted into oauth.json, so a single bad discovery would send the PKCE
// verifier once and the refresh token on every renewal thereafter — forever,
// silently. It had no test before this moved here.
func TestSameSecureOriginRejectsOffOriginEndpoints(t *testing.T) {
	const issuer = "https://api.onesilo.com"
	for _, tc := range []struct {
		name, endpoint string
		wantErr        bool
	}{
		{"same origin", "https://api.onesilo.com/token", false},
		{"different host", "https://evil.example/token", true},
		{"subdomain is a different host", "https://api.onesilo.com.evil.example/token", true},
		{"different scheme", "http://api.onesilo.com/token", true},
		{"different port", "https://api.onesilo.com:8443/token", true},
		{"userinfo smuggling", "https://api.onesilo.com@evil.example/token", true},
		// An explicit default port is the SAME origin — RFC 8414 metadata
		// may legally spell it out, and rejecting it breaks sign-in against
		// a perfectly honest control plane.
		{"explicit default port is same origin", "https://api.onesilo.com:443/token", false},
		// But normalization must not loosen anything: 443 spelled out on
		// http is not http's default, and stays rejected.
		{"default port of the wrong scheme", "http://api.onesilo.com:443/token", true},
		// DNS is case-insensitive; a shouted hostname is not another host.
		{"hostname case", "https://API.ONESILO.COM/token", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := SameSecureOrigin(issuer, tc.endpoint)
			if tc.wantErr && err == nil {
				t.Fatalf("%s was accepted against issuer %s", tc.endpoint, issuer)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%s was rejected: %v", tc.endpoint, err)
			}
		})
	}
}

// Loopback http is tolerated so a developer can run a control plane locally;
// everything else must be https.
func TestSameSecureOriginAllowsLoopbackHTTP(t *testing.T) {
	if err := SameSecureOrigin("http://127.0.0.1:9999", "http://127.0.0.1:9999/token"); err != nil {
		t.Fatalf("loopback http should be allowed for local development: %v", err)
	}
	if err := SameSecureOrigin("http://example.com", "http://example.com/token"); err == nil {
		t.Fatal("plain http on a non-loopback host was accepted")
	}
}

func TestDiscoverRejectsAnOffOriginTokenEndpoint(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Everything on-origin except the token endpoint — the one that
		// receives the refresh token.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         "https://evil.example/token",
			"registration_endpoint":  srv.URL + "/register",
		})
	}))
	defer srv.Close()

	_, err := DiscoverOAuth(context.Background(), srv.Client(), srv.URL)
	if err == nil {
		t.Fatal("a discovery document pointing token_endpoint off-origin was accepted")
	}
	if !strings.Contains(err.Error(), "token_endpoint") {
		t.Errorf("the error should name the offending field, got %q", err)
	}
}

func TestDiscoverRejectsAnIncompleteDocument(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": "x"})
	}))
	defer srv.Close()
	if _, err := DiscoverOAuth(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("a discovery document with no endpoints was accepted")
	}
}

// The full begin → authorize → exchange path against a stand-in control
// plane, asserting the parts that must appear on the wire.
func TestFlowCarriesPKCEAndRedirectURI(t *testing.T) {
	var tokenForm string
	var registerBody string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer":                 srv.URL,
				"authorization_endpoint": srv.URL + "/authorize",
				"token_endpoint":         srv.URL + "/token",
				"registration_endpoint":  srv.URL + "/register",
			})
		case "/register":
			b, _ := io.ReadAll(r.Body)
			registerBody = string(b)
			_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "cid-1"})
		case "/token":
			_ = r.ParseForm()
			tokenForm = r.Form.Encode()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at", "refresh_token": "rt", "expires_in": 3600,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	const redirect = "http://127.0.0.1:8866/v1/controlplane/auth/callback"
	flow, err := BeginFlow(context.Background(), srv.Client(), srv.URL, "test-node", redirect)
	if err != nil {
		t.Fatalf("BeginFlow: %v", err)
	}

	// The redirect URI we will serve must be the one registered, or the
	// control plane will refuse to send the browser back.
	if !strings.Contains(registerBody, redirect) {
		t.Errorf("registration did not carry the redirect URI: %s", registerBody)
	}

	authURL := flow.AuthorizationURL()
	for _, want := range []string{"code_challenge=", "code_challenge_method=S256", "state=", "client_id=cid-1"} {
		if !strings.Contains(authURL, want) {
			t.Errorf("authorization URL missing %q: %s", want, authURL)
		}
	}
	// The verifier is the secret half of PKCE and must never appear in the
	// URL the browser is sent to.
	if strings.Contains(authURL, flow.Verifier) {
		t.Error("the PKCE verifier leaked into the authorization URL")
	}

	cred, err := flow.Exchange(context.Background(), srv.Client(), "the-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if !strings.Contains(tokenForm, "code_verifier=") {
		t.Errorf("the token request carried no PKCE verifier: %s", tokenForm)
	}
	if cred.AccessToken != "at" || cred.RefreshToken != "rt" {
		t.Errorf("credential not populated: %+v", cred)
	}
	if cred.TokenEndpoint != srv.URL+"/token" {
		t.Errorf("token endpoint not recorded for refresh: %q", cred.TokenEndpoint)
	}
	if cred.ExpiresAt.Before(time.Now()) {
		t.Error("expiry should be in the future")
	}
}

func TestFlowExpiry(t *testing.T) {
	f := &Flow{StartedAt: time.Now()}
	if f.Expired(time.Now()) {
		t.Error("a fresh flow reported expired")
	}
	if !f.Expired(time.Now().Add(FlowTimeout + time.Second)) {
		t.Error("a flow past the timeout reported live")
	}
}

func TestClassifySignInErrorMapsRefusalsToUserNotFound(t *testing.T) {
	for _, msg := range []string{
		"user not found", "no such user", "unknown user",
		"account not found", "access_denied: owner declined", "invalid_grant",
	} {
		if !errors.Is(ClassifySignInError(errors.New(msg)), ErrUserNotFound) {
			t.Errorf("%q should map to user-not-found", msg)
		}
	}
	other := errors.New("server_error: database on fire")
	if errors.Is(ClassifySignInError(other), ErrUserNotFound) {
		t.Error("an unrelated failure was misreported as a missing account")
	}
	if ClassifySignInError(nil) != nil {
		t.Error("nil should stay nil")
	}
}
