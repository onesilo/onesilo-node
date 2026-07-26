package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/onesilo/silo-node/internal/controlplane"
)

// signInTimeout bounds how long setup waits for the browser approval.
const signInTimeout = 5 * time.Minute

// errUserNotFound wraps sign-in failures that read as "no account": the
// wizard turns it into a create-an-account pointer.
var errUserNotFound = errors.New("user not found")

// oauthMetadata is the RFC 8414 discovery subset we use.
type oauthMetadata struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

// signIn runs the OAuth authorization-code + PKCE flow against the control
// plane — the same pairing every One Silo MCP client uses — and persists
// the resulting credential to <dataDir>/oauth.json. The node then holds its
// own grant (like the Silo iOS app) and appears as a revocable connection
// in the owner's dashboard.
func signIn(ctx context.Context, controlPlaneURL, dataDir, deviceName string, out io.Writer) error {
	base := strings.TrimRight(controlPlaneURL, "/")
	client := &http.Client{Timeout: 15 * time.Second}

	// 1. Discovery (RFC 8414).
	meta, err := discoverOAuth(ctx, client, base)
	if err != nil {
		return fmt.Errorf("the control plane at %s does not offer OAuth sign-in: %w", base, err)
	}

	// 2. Localhost callback listener on an ephemeral port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("binding sign-in callback listener: %w", err)
	}
	defer ln.Close()
	redirectURI := fmt.Sprintf("http://%s/callback", ln.Addr().String())

	// 3. Dynamic client registration for this node.
	clientID, err := registerClient(ctx, client, meta.RegistrationEndpoint, deviceName, redirectURI)
	if err != nil {
		return fmt.Errorf("registering this node with One Silo: %w", err)
	}

	// 4. Authorization request with PKCE.
	verifier, challenge, err := pkcePair()
	if err != nil {
		return err
	}
	state, err := randomToken(16)
	if err != nil {
		return err
	}
	authURL := meta.AuthorizationEndpoint + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	fmt.Fprintf(out, "  Open this URL to sign in (waiting up to %s):\n\n    %s\n\n", signInTimeout, authURL)
	openBrowser(authURL)

	code, err := waitForCallback(ctx, ln, state)
	if err != nil {
		return err
	}

	// 5. Code exchange.
	cred, err := exchangeCode(ctx, client, meta.TokenEndpoint, clientID, code, verifier, redirectURI)
	if err != nil {
		return err
	}
	return controlplane.SaveOAuthCredential(dataDir, cred)
}

func discoverOAuth(ctx context.Context, client *http.Client, base string) (oauthMetadata, error) {
	var meta oauthMetadata
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
	return meta, nil
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

// waitForCallback serves the redirect URI until the authorization code
// arrives (or the flow errors / times out).
func waitForCallback(ctx context.Context, ln net.Listener, state string) (string, error) {
	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			switch {
			case q.Get("error") != "":
				msg := q.Get("error")
				if d := q.Get("error_description"); d != "" {
					msg += ": " + d
				}
				http.Error(w, "Sign-in failed. Return to the terminal.", http.StatusBadRequest)
				results <- result{err: classifySignInError(errors.New(msg))}
			case q.Get("state") != state:
				http.Error(w, "State mismatch. Return to the terminal.", http.StatusBadRequest)
				results <- result{err: errors.New("state mismatch in sign-in callback")}
			case q.Get("code") == "":
				http.Error(w, "Missing code. Return to the terminal.", http.StatusBadRequest)
				results <- result{err: errors.New("sign-in callback is missing the authorization code")}
			default:
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, "<html><body><h3>Signed in to One Silo.</h3><p>Return to the terminal.</p></body></html>")
				results <- result{code: q.Get("code")}
			}
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	select {
	case r := <-results:
		return r.code, r.err
	case <-time.After(signInTimeout):
		return "", errors.New("timed out waiting for the browser sign-in")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func exchangeCode(ctx context.Context, client *http.Client, tokenEndpoint, clientID, code, verifier, redirectURI string) (controlplane.OAuthCredential, error) {
	var cred controlplane.OAuthCredential
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
		return cred, classifySignInError(errors.New(msg))
	}
	cred = controlplane.OAuthCredential{
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

// classifySignInError maps control-plane rejections that read as "this
// person has no account" onto errUserNotFound so the wizard can point at
// account creation.
func classifySignInError(err error) error {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"user not found", "no such user", "unknown user", "account not found", "access_denied", "invalid_grant"} {
		if strings.Contains(msg, marker) {
			return fmt.Errorf("%w: %s", errUserNotFound, err.Error())
		}
	}
	return err
}

// pkcePair returns a PKCE code verifier and its S256 challenge.
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

// openBrowser best-effort opens the URL in the default browser.
func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "linux":
		cmd = exec.Command("xdg-open", u)
	default:
		return
	}
	_ = cmd.Start()
}
