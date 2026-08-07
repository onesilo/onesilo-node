package adminapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onesilo/onesilo-node/internal/config"
	"github.com/onesilo/onesilo-node/internal/controlplane"
)

type fakeController struct {
	cfg        config.Config
	jwt        string
	pairingKey string
	shutdowns  atomic.Int32
	patchErr   error

	generateErr     error
	lastPrompt      string
	lastTemperature float64

	pendingPairings  []PairingPending
	verifyPairingErr error
	verifiedAccount  string
	verifiedAppIDPub string
}

func (f *fakeController) Status(context.Context) Status {
	return Status{Version: "test", Capabilities: []CapabilityStatus{}}
}
func (f *fakeController) ConfigSnapshot() config.Config { return f.cfg }
func (f *fakeController) ApplyConfigPatch(_ context.Context, p ConfigPatch) (config.Config, error) {
	if f.patchErr != nil {
		return f.cfg, f.patchErr
	}
	p.ApplyTo(&f.cfg)
	return f.cfg, nil
}
func (f *fakeController) SetJWT(token string) { f.jwt = token }
func (f *fakeController) Generate(_ context.Context, prompt string, temperature float64) (string, string, error) {
	if f.generateErr != nil {
		return "", "", f.generateErr
	}
	f.lastPrompt, f.lastTemperature = prompt, temperature
	return "distilled: " + prompt, "test-model:3b", nil
}
func (f *fakeController) SetPairingKey(key string) error { f.pairingKey = key; return nil }
func (f *fakeController) PendingPairings() []PairingPending {
	return f.pendingPairings
}
func (f *fakeController) VerifyPairing(accountID, appIDPub string) error {
	if f.verifyPairingErr != nil {
		return f.verifyPairingErr
	}
	f.verifiedAccount, f.verifiedAppIDPub = accountID, appIDPub
	return nil
}
func (f *fakeController) Shutdown() { f.shutdowns.Add(1) }

func (f *fakeController) OpenAIKeys() []OpenAIKey { return nil }
func (f *fakeController) MintOpenAIKey(name string) (MintedOpenAIKey, error) {
	return MintedOpenAIKey{OpenAIKey: OpenAIKey{ID: "key_test", Name: name}, Key: "silo_sk_test"}, nil
}
func (f *fakeController) RevokeOpenAIKey(id string) error { return nil }

func newTestServer(t *testing.T, token string) (*httptest.Server, *fakeController) {
	t.Helper()
	ctrl := &fakeController{cfg: config.Default()}
	srv := httptest.NewServer(newMux(token, ctrl, slog.Default()))
	t.Cleanup(srv.Close)
	return srv, ctrl
}

func doReq(t *testing.T, method, url, bearer, body string) *http.Response {
	t.Helper()
	var reqBody *strings.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	} else {
		reqBody = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHealthzIsOpen(t *testing.T) {
	srv, _ := newTestServer(t, "secret-token")
	resp := doReq(t, http.MethodGet, srv.URL+"/healthz", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz without auth = %d, want 200", resp.StatusCode)
	}
}

func TestStatusRequiresToken(t *testing.T) {
	srv, _ := newTestServer(t, "secret-token")

	if resp := doReq(t, http.MethodGet, srv.URL+"/v1/status", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", resp.StatusCode)
	}
	if resp := doReq(t, http.MethodGet, srv.URL+"/v1/status", "wrong-token", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", resp.StatusCode)
	}
	// A prefix of the real token must also fail (constant-time compare over
	// hashes, not a length-leaky prefix match).
	if resp := doReq(t, http.MethodGet, srv.URL+"/v1/status", "secret", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("prefix token = %d, want 401", resp.StatusCode)
	}
	if resp := doReq(t, http.MethodGet, srv.URL+"/v1/status", "secret-token", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("correct token = %d, want 200", resp.StatusCode)
	}
}

func TestEmptyConfiguredTokenFailsClosed(t *testing.T) {
	srv, _ := newTestServer(t, "")
	if resp := doReq(t, http.MethodGet, srv.URL+"/v1/status", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("empty configured token, no header = %d, want 401", resp.StatusCode)
	}
	// Even presenting an empty bearer must not match an empty configured token.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer ")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("empty bearer vs empty token = %d, want 401", resp.StatusCode)
	}
}

func TestConfigPatchApplies(t *testing.T) {
	srv, ctrl := newTestServer(t, "tok")
	resp := doReq(t, http.MethodPut, srv.URL+"/v1/config", "tok",
		`{"capabilities":{"compute":true},"ollama":{"default_model":"qwen2.5:7b"}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /v1/config = %d", resp.StatusCode)
	}
	if !ctrl.cfg.Capabilities.Compute {
		t.Error("compute not enabled by patch")
	}
	if ctrl.cfg.Ollama.DefaultModel != "qwen2.5:7b" {
		t.Errorf("default_model = %q", ctrl.cfg.Ollama.DefaultModel)
	}
	// Untouched fields keep their values.
	if ctrl.cfg.Admin.Port != 8766 {
		t.Errorf("admin port changed unexpectedly: %d", ctrl.cfg.Admin.Port)
	}
}

func TestConfigPatchRejectsUnknownField(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	resp := doReq(t, http.MethodPut, srv.URL+"/v1/config", "tok", `{"bogus":true}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400", resp.StatusCode)
	}
}

func TestConfigPatchValidationError(t *testing.T) {
	srv, ctrl := newTestServer(t, "tok")
	ctrl.patchErr = fmt.Errorf("tunnel.mode must be one of off|quick|external")
	resp := doReq(t, http.MethodPut, srv.URL+"/v1/config", "tok", `{"tunnel":{"mode":"sideways"}}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid patch = %d, want 422", resp.StatusCode)
	}
}

func TestJWTPush(t *testing.T) {
	srv, ctrl := newTestServer(t, "tok")
	resp := doReq(t, http.MethodPost, srv.URL+"/v1/auth/jwt", "tok", `{"token":"eyJhbGciOi..."}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("jwt push = %d", resp.StatusCode)
	}
	if ctrl.jwt != "eyJhbGciOi..." {
		t.Errorf("jwt = %q", ctrl.jwt)
	}
	if resp := doReq(t, http.MethodPost, srv.URL+"/v1/auth/jwt", "tok", `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty token = %d, want 400", resp.StatusCode)
	}
}

func TestPairingKeyValidation(t *testing.T) {
	srv, ctrl := newTestServer(t, "tok")
	valid := strings.Repeat("ab", 32) // 64 hex chars
	resp := doReq(t, http.MethodPost, srv.URL+"/v1/auth/pairing-key", "tok", `{"pairing_key":"`+valid+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid pairing key = %d", resp.StatusCode)
	}
	if ctrl.pairingKey != valid {
		t.Errorf("pairing key not stored")
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 64), strings.Repeat("a", 63)} {
		resp := doReq(t, http.MethodPost, srv.URL+"/v1/auth/pairing-key", "tok", `{"pairing_key":"`+bad+`"}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("pairing key %q = %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestShutdown(t *testing.T) {
	srv, ctrl := newTestServer(t, "tok")
	resp := doReq(t, http.MethodPost, srv.URL+"/v1/shutdown", "tok", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("shutdown = %d, want 202", resp.StatusCode)
	}
	// The shutdown fires on a goroutine after the response; poll briefly.
	for i := 0; i < 200 && ctrl.shutdowns.Load() == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	if ctrl.shutdowns.Load() == 0 {
		t.Error("Shutdown was not invoked")
	}
}

// Control-plane OAuth surface: not exercised by these tests, present so the
// fake satisfies Controller.
func (f *fakeController) DeviceName() string { return "test-node" }
func (f *fakeController) ControlPlaneCredential() (controlplane.OAuthCredential, error) {
	return controlplane.OAuthCredential{}, controlplane.ErrNotSignedIn
}
func (f *fakeController) SaveControlPlaneCredential(controlplane.OAuthCredential) error { return nil }
func (f *fakeController) DisconnectControlPlane() error                                 { return nil }
