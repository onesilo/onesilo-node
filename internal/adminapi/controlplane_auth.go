package adminapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/onesilo/onesilo-node/internal/controlplane"
)

// Connecting the node to One Silo from the browser.
//
// The console previously dead-ended here: it could see that the node was not
// signed in and told the operator to go run `onesilo-node setup` in a
// terminal. That is a strange thing to ask of someone already looking at the
// node's own admin UI, and on a headless box it is worse than strange.
//
// The shape is an ordinary authorization-code flow with one property worth
// stating plainly, because it decides the security of the whole thing: the
// callback CANNOT be authenticated with the admin bearer token. It arrives
// as a browser redirect from the control plane, and a redirect carries no
// Authorization header. So `state` is doing the entire job of proving that a
// callback belongs to a flow this node started — which makes it a secret,
// single-use, expiring, and compared in constant time.
//
// The admin server binds loopback only, so the browser is necessarily on the
// node's own machine and can serve as the redirect target. That is what lets
// the redirect URI be stable (unlike setup's ephemeral port) and lets the
// console take over the page after the exchange.

// callbackPath is registered with the control plane as this client's only
// redirect URI, so it must match the route exactly.
const callbackPath = "/v1/controlplane/auth/callback"

// pendingFlows holds the authorization that has been started and not yet
// completed — at most ONE, by design, not a map that happens to be small.
//
// A single slot is the bound: there is exactly one human at this console,
// so a second "Connect" click means they gave up on the first attempt, and
// the new flow replaces it. That is also what keeps abandoned PKCE
// verifiers from accumulating — the previous one is dropped at the moment
// it stopped being wanted, and take() consumes the slot on every outcome,
// success or not.
type pendingFlows struct {
	mu   sync.Mutex
	flow *controlplane.Flow
}

func (p *pendingFlows) put(f *controlplane.Flow) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flow = f
}

// take returns the pending flow if state matches, and consumes it either
// way. Consuming on mismatch is deliberate: a wrong state is either a stale
// tab or someone guessing, and neither should get a second attempt against
// the same verifier.
func (p *pendingFlows) take(state string) (*controlplane.Flow, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.flow
	p.flow = nil
	if f == nil {
		return nil, errors.New("no sign-in is in progress — start again from the admin console")
	}
	if f.Expired(time.Now()) {
		return nil, fmt.Errorf("this sign-in expired after %s — start again", controlplane.FlowTimeout)
	}
	// Constant time: state is the only thing standing between a stray
	// request and a completed sign-in, so it is compared like a secret.
	if subtle.ConstantTimeCompare([]byte(f.State), []byte(state)) != 1 {
		return nil, errors.New("this sign-in link does not match the one this node started")
	}
	return f, nil
}

func (p *pendingFlows) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flow = nil
}

// ControlPlaneAuthStatus is the GET /v1/controlplane/auth response.
type ControlPlaneAuthStatus struct {
	// Connected is whether a usable credential is stored.
	Connected bool `json:"connected"`
	// ControlPlaneURL is who the node would connect to.
	ControlPlaneURL string `json:"control_plane_url"`
	// ExpiresAt is the access token's expiry, if the control plane gave
	// one. A past value is normal and not a problem: the refresh token
	// renews it. Surfaced so the console can say "connected" honestly
	// rather than implying the access token is live.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// CanRefresh is whether a refresh token was issued. Without one the
	// connection dies at expiry and needs a fresh sign-in, which is worth
	// showing before it happens.
	CanRefresh bool `json:"can_refresh"`
	// SignInInProgress reports an authorization awaiting the browser.
	SignInInProgress bool `json:"sign_in_in_progress"`
}

func registerControlPlaneAuthRoutes(
	authed func(string, http.HandlerFunc),
	mux *http.ServeMux,
	ctrl Controller,
	logger *slog.Logger,
) {
	pending := &pendingFlows{}

	authed("GET /v1/controlplane/auth", func(w http.ResponseWriter, r *http.Request) {
		status := ControlPlaneAuthStatus{
			ControlPlaneURL: ctrl.ConfigSnapshot().ControlPlane.URL,
		}
		pending.mu.Lock()
		status.SignInInProgress = pending.flow != nil && !pending.flow.Expired(time.Now())
		pending.mu.Unlock()

		cred, err := ctrl.ControlPlaneCredential()
		if err == nil && cred.AccessToken != "" {
			status.Connected = true
			status.CanRefresh = cred.RefreshToken != ""
			if !cred.ExpiresAt.IsZero() {
				at := cred.ExpiresAt
				status.ExpiresAt = &at
			}
		}
		writeJSON(w, http.StatusOK, status)
	})

	authed("POST /v1/controlplane/auth/start", func(w http.ResponseWriter, r *http.Request) {
		cfg := ctrl.ConfigSnapshot()
		// The redirect URI must be an address the control plane will send a
		// browser back to, and it must match what we register. Derived from
		// the admin port rather than r.Host so a request that arrived via
		// some other name cannot register a redirect pointing elsewhere.
		redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Admin.Port, callbackPath)

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		flow, err := controlplane.BeginFlow(
			ctx,
			&http.Client{Timeout: 15 * time.Second},
			cfg.ControlPlane.URL,
			ctrl.DeviceName(),
			redirectURI,
		)
		if err != nil {
			// The control plane being unreachable or not offering OAuth is
			// an upstream condition, not a bad request from the console.
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		pending.put(flow)
		logger.Info("control-plane sign-in started via admin console",
			"control_plane", cfg.ControlPlane.URL)
		writeJSON(w, http.StatusOK, map[string]any{
			"authorization_url": flow.AuthorizationURL(),
			"expires_in":        int(controlplane.FlowTimeout.Seconds()),
		})
	})

	authed("DELETE /v1/controlplane/auth", func(w http.ResponseWriter, r *http.Request) {
		pending.clear()
		if err := ctrl.DisconnectControlPlane(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		logger.Info("control-plane credential removed via admin console")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	// NOT behind requireAdminToken: this is a browser redirect from the
	// control plane and cannot carry a bearer token. `state` is the
	// authentication — see pendingFlows.take. Registered straight on the
	// mux for that reason, so the exception is visible here rather than
	// hidden in a wrapper.
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// An error from the control plane (the human hit "cancel") arrives
		// here too, and must not be reported as a node failure.
		if e := q.Get("error"); e != "" {
			pending.clear()
			desc := q.Get("error_description")
			if desc == "" {
				desc = e
			}
			logger.Info("control-plane sign-in was refused", "error", e)
			writeCallbackPage(w, http.StatusOK, false, "Sign-in was cancelled or refused: "+desc)
			return
		}

		flow, err := pending.take(q.Get("state"))
		if err != nil {
			logger.Warn("rejected a control-plane sign-in callback", "reason", err.Error())
			writeCallbackPage(w, http.StatusBadRequest, false, err.Error())
			return
		}
		code := q.Get("code")
		if code == "" {
			writeCallbackPage(w, http.StatusBadRequest, false, "the control plane returned no authorization code")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		cred, err := flow.Exchange(ctx, &http.Client{Timeout: 15 * time.Second}, code)
		if err != nil {
			// Deliberately not logging the code or the error verbatim at
			// error level with the URL attached — the query string holds a
			// live authorization code until it is redeemed.
			logger.Error("control-plane code exchange failed", "error", err.Error())
			msg := err.Error()
			if errors.Is(err, controlplane.ErrUserNotFound) {
				msg = "No One Silo account matched that sign-in. Create one, then connect again."
			}
			writeCallbackPage(w, http.StatusBadGateway, false, msg)
			return
		}
		if err := ctrl.SaveControlPlaneCredential(cred); err != nil {
			logger.Error("storing the control-plane credential failed", "error", err.Error())
			writeCallbackPage(w, http.StatusInternalServerError, false, err.Error())
			return
		}
		logger.Info("node connected to the control plane via admin console")
		writeCallbackPage(w, http.StatusOK, true, "")
	})
}

// writeCallbackPage renders the browser tab the human is left looking at.
//
// Served as HTML rather than redirected back to the console: a redirect
// would carry this page's query string — which contains a live authorization
// code — into the console's URL, browser history, and any referrer. The
// exchange is already done by the time this renders, so there is nothing to
// hand back except the outcome.
func writeCallbackPage(w http.ResponseWriter, status int, ok bool, detail string) {
	title := "Connected to One Silo"
	body := "This node is now connected. You can close this tab and return to the admin console."
	if !ok {
		title = "Could not connect"
		body = detail
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The detail can include text from the control plane, so it is escaped.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>%s</title>
<style>
 body{font:16px/1.5 system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;background:#0f1115;color:#e6e8ee}
 .card{max-width:32rem;padding:2rem;border:1px solid #262b36;border-radius:12px;background:#151922}
 h1{font-size:1.25rem;margin:0 0 .5rem}
 p{margin:0;color:#aeb4c2}
 .bad h1{color:#ff8f8f}
</style></head>
<body><div class="card%s"><h1>%s</h1><p>%s</p></div>
<script>
// Strip the authorization code from the address bar so it does not linger
// in history or get shoulder-read. The exchange has already happened.
if (window.history && window.history.replaceState) {
  window.history.replaceState({}, "", window.location.pathname);
}
</script>
</body></html>`,
		html.EscapeString(title),
		map[bool]string{true: "", false: " bad"}[ok],
		html.EscapeString(title),
		html.EscapeString(body),
	)
}
