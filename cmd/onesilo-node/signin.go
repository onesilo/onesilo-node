package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/onesilo/onesilo-node/internal/controlplane"
)

// signInTimeout bounds how long setup waits for the browser approval. It
// is deliberately generous: the Clerk-hosted page lets a new user create
// their account mid-flow (sign-up + email verification) before approving.
const signInTimeout = 10 * time.Minute

// errUserNotFound wraps sign-in failures that read as "no account": the
// wizard turns it into a create-an-account pointer. Aliased to the shared
// definition so `errors.Is` keeps matching across the move.
var errUserNotFound = controlplane.ErrUserNotFound

// signIn runs the OAuth authorization-code + PKCE flow against the control
// plane — the same pairing every One Silo MCP client uses — and persists
// the resulting credential to <dataDir>/oauth.json. The node then holds its
// own grant (like the Silo iOS app) and appears as a revocable connection
// in the owner's dashboard.
func signIn(ctx context.Context, controlPlaneURL, dataDir, deviceName string, out io.Writer) error {
	client := &http.Client{Timeout: 15 * time.Second}

	// Loopback callback listener on an ephemeral port. This is the one
	// thing setup does differently from the admin console, which serves the
	// callback from the admin server it already runs — hence the split:
	// everything security-relevant is shared, only the transport differs.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("binding sign-in callback listener: %w", err)
	}
	defer ln.Close()
	redirectURI := fmt.Sprintf("http://%s/callback", ln.Addr().String())

	flow, err := controlplane.BeginFlow(ctx, client, controlPlaneURL, deviceName, redirectURI)
	if err != nil {
		return err
	}

	authURL := flow.AuthorizationURL()
	fmt.Fprintf(out, "  Sign in — or create your account — in the browser (waiting up to %s):\n\n    %s\n\n", signInTimeout, authURL)
	openBrowser(authURL)

	code, err := waitForCallback(ctx, ln, flow.State)
	if err != nil {
		return err
	}

	cred, err := flow.Exchange(ctx, client, code)
	if err != nil {
		return err
	}
	return controlplane.SaveOAuthCredential(dataDir, cred)
}

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
				results <- result{err: controlplane.ClassifySignInError(errors.New(msg))}
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
