package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onesilo/onesilo-node/internal/controlplane"
)

// syncBuffer lets the "browser" goroutine poll wizard output safely.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeControlPlane implements discovery, DCR, and the token endpoint.
type fakeControlPlane struct {
	srv          *httptest.Server
	mu           sync.Mutex
	clientName   string
	seenVerifier string
}

func newFakeControlPlane(t *testing.T) *fakeControlPlane {
	t.Helper()
	f := &fakeControlPlane{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"authorization_endpoint": f.srv.URL + "/oauth/authorize",
			"token_endpoint":         f.srv.URL + "/oauth/token",
			"registration_endpoint":  f.srv.URL + "/oauth/register",
		})
	})
	mux.HandleFunc("POST /oauth/register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ClientName string `json:"client_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.clientName = body.ClientName
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"client_id": "node-client-1"})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.seenVerifier = r.Form.Get("code_verifier")
		f.mu.Unlock()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "test-code" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-node", "refresh_token": "rt-node", "expires_in": 3600,
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

var authURLPattern = regexp.MustCompile(`https?://\S+/oauth/authorize\?\S+`)

// approve acts as the browser: waits for the auth URL in the wizard output,
// then hits the redirect URI with a code (or an error).
func approve(t *testing.T, out *syncBuffer, callbackQuery func(url.Values) url.Values) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := authURLPattern.FindString(out.String()); m != "" {
			u, err := url.Parse(m)
			if err != nil {
				t.Errorf("bad auth URL %q: %v", m, err)
				return
			}
			q := u.Query()
			if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
				t.Error("authorization request must carry a PKCE S256 challenge")
			}
			cb, _ := url.Parse(q.Get("redirect_uri"))
			cb.RawQuery = callbackQuery(q).Encode()
			resp, err := http.Get(cb.String())
			if err != nil {
				t.Errorf("callback request failed: %v", err)
				return
			}
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("never saw the authorization URL in wizard output")
}

func TestSignInHappyPath(t *testing.T) {
	f := newFakeControlPlane(t)
	dir := t.TempDir()
	out := &syncBuffer{}

	go approve(t, out, func(q url.Values) url.Values {
		return url.Values{"code": {"test-code"}, "state": {q.Get("state")}}
	})

	if err := signIn(t.Context(), f.srv.URL, dir, "test-mac", out); err != nil {
		t.Fatalf("sign-in failed: %v", err)
	}

	cred, err := controlplane.LoadOAuthCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != "at-node" || cred.RefreshToken != "rt-node" || cred.ClientID != "node-client-1" {
		t.Fatalf("unexpected credential %+v", cred)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clientName != "Silo Node (test-mac)" {
		t.Fatalf("unexpected registered client name %q", f.clientName)
	}
	if f.seenVerifier == "" {
		t.Fatal("token exchange must carry the PKCE verifier")
	}
}

func TestSignInDeniedMapsToUserNotFound(t *testing.T) {
	f := newFakeControlPlane(t)
	out := &syncBuffer{}

	go approve(t, out, func(q url.Values) url.Values {
		return url.Values{"error": {"access_denied"}, "state": {q.Get("state")}}
	})

	err := signIn(t.Context(), f.srv.URL, t.TempDir(), "test-mac", out)
	if !errors.Is(err, errUserNotFound) {
		t.Fatalf("expected errUserNotFound, got %v", err)
	}
}

func TestSignInRejectsStateMismatch(t *testing.T) {
	f := newFakeControlPlane(t)
	out := &syncBuffer{}

	go approve(t, out, func(q url.Values) url.Values {
		return url.Values{"code": {"test-code"}, "state": {"forged"}}
	})

	err := signIn(t.Context(), f.srv.URL, t.TempDir(), "test-mac", out)
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("expected a state mismatch error, got %v", err)
	}
}

func TestClassifySignInError(t *testing.T) {
	for _, msg := range []string{"User not found", "access_denied: owner declined", "invalid_grant"} {
		if !errors.Is(controlplane.ClassifySignInError(errors.New(msg)), errUserNotFound) {
			t.Errorf("%q should map to user-not-found", msg)
		}
	}
	if errors.Is(controlplane.ClassifySignInError(errors.New("network is down")), errUserNotFound) {
		t.Error("unrelated errors must not map to user-not-found")
	}
}
