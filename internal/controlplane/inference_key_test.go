package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubKeys is an InferenceKeyProvider whose mint and commit are observable.
type stubKeys struct {
	key       string
	err       error
	minted    int
	committed int
}

func (s *stubKeys) Mint() (string, func(), error) {
	s.minted++
	if s.err != nil {
		return "", nil, s.err
	}
	return s.key, func() { s.committed++ }, nil
}

// registerWith runs one register() against a server that captures the body
// and answers with status, and returns what the body contained.
func registerWith(t *testing.T, keys InferenceKeyProvider, status int) map[string]any {
	t.Helper()
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Write([]byte(`{"device_id":"11111111-2222-3333-4444-555555555555",` +
			`"heartbeat_interval_seconds":30,"ttl_seconds":90}`))
	}))
	defer srv.Close()

	m := NewManager(
		newTestClient(srv.URL, staticToken("tok")),
		t.TempDir(),
		func() string { return "dev" },
		func() string { return "" },
		func() string { return "" },
		func() []CapabilityProbe { return nil },
		keys,
		slog.New(slog.DiscardHandler),
	)
	m.register(context.Background(), "https://x.trycloudflare.com", []string{"llm_inference"})
	return captured
}

func TestRegisterPublishesTheMintedInferenceKey(t *testing.T) {
	keys := &stubKeys{key: "silo_sk_abc123"}
	body := registerWith(t, keys, http.StatusOK)

	if got := body["inference_key"]; got != "silo_sk_abc123" {
		t.Errorf("inference_key = %v, want the minted key", got)
	}
	if keys.minted != 1 {
		t.Errorf("minted %d times, want 1", keys.minted)
	}
}

func TestASuccessfulRegistrationCommitsTheKey(t *testing.T) {
	// Committing is what retires the key this one replaces, so it must
	// happen exactly when the control plane has the new one.
	keys := &stubKeys{key: "silo_sk_abc123"}
	registerWith(t, keys, http.StatusOK)
	if keys.committed != 1 {
		t.Errorf("committed %d times after a 200, want 1", keys.committed)
	}
}

func TestAFailedRegistrationDoesNotCommitTheKey(t *testing.T) {
	// The superseded key is still the one the control plane holds. Retiring
	// it here would break inference for as long as registration kept
	// failing -- exactly when the node can least fix it.
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusNotFound,
	} {
		keys := &stubKeys{key: "silo_sk_abc123"}
		registerWith(t, keys, status)
		if keys.committed != 0 {
			t.Errorf("status %d: committed %d times, want 0", status, keys.committed)
		}
	}
}

func TestAnEmptyKeyIsOmittedRatherThanSentBlank(t *testing.T) {
	// The backend reads an absent field as "keep the key you have". Sending
	// "" would look like a value and could be stored over a working key.
	keys := &stubKeys{key: ""}
	body := registerWith(t, keys, http.StatusOK)
	if _, present := body["inference_key"]; present {
		t.Errorf("inference_key present in body when nothing was minted: %v", body["inference_key"])
	}
}

func TestANilProviderPublishesNothingAndStillRegisters(t *testing.T) {
	body := registerWith(t, nil, http.StatusOK)
	if _, present := body["inference_key"]; present {
		t.Error("inference_key present with no provider configured")
	}
	if body["tunnel_url"] != "https://x.trycloudflare.com" {
		t.Errorf("registration did not happen: %v", body)
	}
}

func TestAMintFailureStillRegistersTheNode(t *testing.T) {
	// A node that cannot mint is still a node: it serves its LAN, answers
	// the app, and reports its capabilities. Only the control plane calling
	// in is lost, and the backend names that fix specifically.
	keys := &stubKeys{err: errors.New("disk full")}
	body := registerWith(t, keys, http.StatusOK)
	if _, present := body["inference_key"]; present {
		t.Error("inference_key present after a failed mint")
	}
	if body["tunnel_url"] != "https://x.trycloudflare.com" {
		t.Errorf("a mint failure blocked registration: %v", body)
	}
}

func TestA422RetryDropsTheInferenceKeyToo(t *testing.T) {
	// A pre-rollout server 422s on a field it does not know, and which one
	// is not distinguishable from the status. Retrying with the key still
	// attached would 422 again and the node would never register.
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		if _, has := body["inference_key"]; has {
			w.WriteHeader(422)
			return
		}
		w.Write([]byte(`{"device_id":"11111111-2222-3333-4444-555555555555"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, staticToken("tok"))
	if _, err := c.Register(context.Background(), RegisterRequest{
		TunnelURL:    "https://x.trycloudflare.com",
		Capabilities: []string{"llm_inference"},
		InferenceKey: "silo_sk_abc123",
	}); err != nil {
		t.Fatalf("Register did not recover from a 422: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(bodies))
	}
	if _, has := bodies[1]["inference_key"]; has {
		t.Error("retry still carried inference_key")
	}
}

func TestAnEmptyKeyWithACommitIsNotCommitted(t *testing.T) {
	// "Publish nothing" and "publish this" are different outcomes, and only
	// the second may retire what it replaces. A provider that hands back a
	// commit alongside an empty key must not be able to make the manager
	// revoke the control plane's working key while sending no replacement.
	keys := &stubEmptyKeyWithCommit{}
	body := registerWith(t, keys, http.StatusOK)

	if _, present := body["inference_key"]; present {
		t.Error("inference_key present when nothing was minted")
	}
	if keys.committed != 0 {
		t.Errorf("committed %d times for an empty key, want 0", keys.committed)
	}
}

// stubEmptyKeyWithCommit publishes nothing but still offers a commit — the
// shape the manager must refuse to act on.
type stubEmptyKeyWithCommit struct{ committed int }

func (s *stubEmptyKeyWithCommit) Mint() (string, func(), error) {
	return "", func() { s.committed++ }, nil
}
