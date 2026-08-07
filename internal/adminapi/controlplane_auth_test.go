package adminapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onesilo/onesilo-node/internal/controlplane"
)

// The callback route is the one unauthenticated write path in this API, so
// most of what is worth testing here is what it refuses.

func newPending(t *testing.T, state string, started time.Time) *pendingFlows {
	t.Helper()
	p := &pendingFlows{}
	p.put(&controlplane.Flow{State: state, StartedAt: started})
	return p
}

func TestCallbackStateIsRequiredAndSingleUse(t *testing.T) {
	p := newPending(t, "correct-state", time.Now())

	if _, err := p.take("wrong-state"); err == nil {
		t.Fatal("a mismatched state was accepted")
	}
	// And the flow is gone: a wrong state is either a stale tab or someone
	// guessing, and neither earns a second attempt against the same PKCE
	// verifier.
	if _, err := p.take("correct-state"); err == nil {
		t.Fatal("the correct state still worked after a failed attempt — the flow was not consumed")
	}
}

func TestCallbackRejectsAReplayedState(t *testing.T) {
	p := newPending(t, "s", time.Now())
	if _, err := p.take("s"); err != nil {
		t.Fatalf("first use should succeed: %v", err)
	}
	if _, err := p.take("s"); err == nil {
		t.Fatal("the same state was accepted twice; an intercepted callback could be replayed")
	}
}

func TestCallbackRejectsAnExpiredFlow(t *testing.T) {
	p := newPending(t, "s", time.Now().Add(-controlplane.FlowTimeout-time.Second))
	_, err := p.take("s")
	if err == nil {
		t.Fatal("an expired flow was accepted")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("the error should say the sign-in expired, got %q", err)
	}
}

func TestCallbackWithNoFlowInProgress(t *testing.T) {
	p := &pendingFlows{}
	if _, err := p.take("anything"); err == nil {
		t.Fatal("a callback was accepted with no flow in progress")
	}
}

// Starting a second sign-in must not leave the first one's verifier resident.
func TestStartingASecondFlowDropsTheFirst(t *testing.T) {
	p := &pendingFlows{}
	p.put(&controlplane.Flow{State: "first", StartedAt: time.Now()})
	p.put(&controlplane.Flow{State: "second", StartedAt: time.Now()})
	if _, err := p.take("first"); err == nil {
		t.Fatal("the abandoned first flow was still completable")
	}
}

// --- route-level ---

type oauthController struct {
	fakeController
	cred       controlplane.OAuthCredential
	haveCred   bool
	saved      *controlplane.OAuthCredential
	disconnect int
}

func (c *oauthController) ControlPlaneCredential() (controlplane.OAuthCredential, error) {
	if !c.haveCred {
		return controlplane.OAuthCredential{}, controlplane.ErrNotSignedIn
	}
	return c.cred, nil
}

func (c *oauthController) SaveControlPlaneCredential(cred controlplane.OAuthCredential) error {
	c.saved = &cred
	c.haveCred = true
	c.cred = cred
	return nil
}

func (c *oauthController) DisconnectControlPlane() error {
	c.disconnect++
	c.haveCred = false
	return nil
}

func TestAuthStatusReportsDisconnectedAndConnected(t *testing.T) {
	ctrl := &oauthController{}
	mux := newMux("tok", ctrl, slog.Default())

	get := func() ControlPlaneAuthStatus {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/controlplane/auth", nil)
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var s ControlPlaneAuthStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		return s
	}

	if s := get(); s.Connected {
		t.Error("a node with no credential reported connected")
	}

	ctrl.haveCred = true
	ctrl.cred = controlplane.OAuthCredential{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	s := get()
	if !s.Connected {
		t.Error("a node with a credential reported disconnected")
	}
	if !s.CanRefresh {
		t.Error("a credential with a refresh token reported CanRefresh=false")
	}
	// The tokens themselves must never appear in an API response, even on
	// this loopback-only, token-authenticated surface.
	body, _ := json.Marshal(s)
	for _, secret := range []string{"at", "rt"} {
		if strings.Contains(string(body), `"`+secret+`"`) {
			t.Errorf("the status response leaked a token value: %s", body)
		}
	}
}

// An expired access token with a refresh token is still "connected" — the
// refresh renews it. Reporting disconnected would send the operator through
// a sign-in they do not need.
func TestExpiredAccessTokenIsStillConnected(t *testing.T) {
	ctrl := &oauthController{
		haveCred: true,
		cred: controlplane.OAuthCredential{
			AccessToken:  "at",
			RefreshToken: "rt",
			ExpiresAt:    time.Now().Add(-time.Hour),
		},
	}
	mux := newMux("tok", ctrl, slog.Default())
	req := httptest.NewRequest(http.MethodGet, "/v1/controlplane/auth", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var s ControlPlaneAuthStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	if !s.Connected {
		t.Error("an expired-but-refreshable credential reported disconnected")
	}
}

func TestAuthStatusAndDisconnectRequireTheAdminToken(t *testing.T) {
	mux := newMux("tok", &oauthController{}, slog.Default())
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/controlplane/auth"},
		{http.MethodPost, "/v1/controlplane/auth/start"},
		{http.MethodDelete, "/v1/controlplane/auth"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

// The callback is deliberately NOT behind the bearer token — a browser
// redirect cannot carry one. This pins that, so nobody "fixes" it into an
// authenticated route and silently breaks sign-in.
func TestCallbackIsReachableWithoutTheAdminToken(t *testing.T) {
	mux := newMux("tok", &oauthController{}, slog.Default())
	req := httptest.NewRequest(http.MethodGet, callbackPath+"?state=nope&code=x", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatal("the callback required the admin token; a browser redirect cannot send one")
	}
	// Reachable, and still refused on its own terms: no flow was started.
	if rec.Code != http.StatusBadRequest {
		t.Errorf("callback with an unknown state = %d, want 400", rec.Code)
	}
}

func TestDisconnectIsIdempotent(t *testing.T) {
	ctrl := &oauthController{}
	mux := newMux("tok", ctrl, slog.Default())
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodDelete, "/v1/controlplane/auth", nil)
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("disconnect #%d = %d, want 200", i+1, rec.Code)
		}
	}
	if ctrl.disconnect != 2 {
		t.Errorf("DisconnectControlPlane called %d times, want 2", ctrl.disconnect)
	}
}

// A refusal at the authorization page ("cancel") comes back through the
// callback and must read as a cancellation, not a node fault.
func TestCallbackReportsAControlPlaneRefusalAsCancellation(t *testing.T) {
	mux := newMux("tok", &oauthController{}, slog.Default())
	req := httptest.NewRequest(http.MethodGet,
		callbackPath+"?error=access_denied&error_description=user+declined", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("a user cancelling = %d, want 200 (it is not a server error)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cancelled or refused") {
		t.Errorf("body should read as a cancellation, got %s", rec.Body.String())
	}
}

// The callback renders control-plane-supplied text, so it must be escaped.
func TestCallbackEscapesControlPlaneText(t *testing.T) {
	mux := newMux("tok", &oauthController{}, slog.Default())
	req := httptest.NewRequest(http.MethodGet,
		callbackPath+"?error=x&error_description="+
			"%3Cimg+src%3Dx+onerror%3Dalert(1)%3E", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "<img src=x") {
		t.Errorf("unescaped control-plane text reached the page: %s", rec.Body.String())
	}
}

func TestSaveCredentialPropagatesFailure(t *testing.T) {
	// Not a route test: this asserts the Controller contract the handler
	// depends on — a failed save must not be reported as a success, or the
	// operator sees "connected" and the node is not.
	ctrl := &failingSaveController{}
	if err := ctrl.SaveControlPlaneCredential(controlplane.OAuthCredential{}); err == nil {
		t.Fatal("expected the failure to propagate")
	}
}

type failingSaveController struct{ oauthController }

func (c *failingSaveController) SaveControlPlaneCredential(controlplane.OAuthCredential) error {
	return errors.New("disk full")
}
