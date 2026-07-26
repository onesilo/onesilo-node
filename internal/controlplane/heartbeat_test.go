package controlplane

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRegisterPaymentRequired verifies a 402 from the destinations API (remote
// access needs a paid plan) backs off on a long interval, flags the manager,
// and leaves the node unregistered — it keeps serving locally.
func TestRegisterPaymentRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer srv.Close()

	m := NewManager(
		newTestClient(srv.URL, staticToken("tok")),
		t.TempDir(),
		func() string { return "dev" },
		func() string { return "" },
		func() string { return "" },
		func() []CapabilityProbe { return nil },
		slog.New(slog.DiscardHandler),
	)

	wait := m.register(context.Background(), "https://x.trycloudflare.com", []string{"llm_inference"})
	if wait != subscriptionRetryInterval {
		t.Fatalf("want backoff %v, got %v", subscriptionRetryInterval, wait)
	}
	if !m.SubscriptionBlocked() {
		t.Fatal("expected SubscriptionBlocked() true after a 402")
	}
	if registered, _ := m.Status(); registered {
		t.Fatal("node should not be registered after a 402")
	}
}

type fakeProbe struct {
	name    string
	enabled bool
	healthy bool
}

func (p *fakeProbe) Name() string  { return p.name }
func (p *fakeProbe) Enabled() bool { return p.enabled }
func (p *fakeProbe) Healthy(context.Context) (bool, string) {
	if p.healthy {
		return true, "ok"
	}
	return false, "down"
}

func newProbeManager(probes ...CapabilityProbe) *Manager {
	return NewManager(
		newTestClient("http://127.0.0.1:1", staticToken("tok")),
		"",
		func() string { return "test-device" },
		func() string { return "" },
		func() string { return "" },
		func() []CapabilityProbe { return probes },
		slog.Default(),
	)
}

func TestCapabilitiesStatusMapping(t *testing.T) {
	m := newProbeManager(
		&fakeProbe{name: "compute", enabled: true, healthy: true},
		&fakeProbe{name: "memory", enabled: true, healthy: false},
	)
	got := m.capabilitiesStatus(context.Background())
	want := map[string]string{
		"llm_inference": "live",
		"silo_recall":   "dead",
		"silo_remember": "dead",
	}
	if len(got) != len(want) {
		t.Fatalf("status = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("status[%s] = %q, want %q", k, got[k], v)
		}
	}
}

func TestCapabilitiesStatusOmitsDisabled(t *testing.T) {
	m := newProbeManager(
		&fakeProbe{name: "compute", enabled: true, healthy: true},
		&fakeProbe{name: "memory", enabled: false, healthy: true},
	)
	got := m.capabilitiesStatus(context.Background())
	if len(got) != 1 || got["llm_inference"] != "live" {
		t.Fatalf("status = %v (memory must not report while disabled)", got)
	}
	if _, has := got["silo_recall"]; has {
		t.Error("silo_recall reported while memory disabled")
	}
}

func TestCapabilitiesStatusAllDisabledIsNil(t *testing.T) {
	m := newProbeManager(
		&fakeProbe{name: "compute", enabled: false},
		&fakeProbe{name: "memory", enabled: false},
	)
	if got := m.capabilitiesStatus(context.Background()); got != nil {
		t.Fatalf("status = %v, want nil", got)
	}
}

func TestEnabledCapabilityIDs(t *testing.T) {
	m := newProbeManager(
		&fakeProbe{name: "compute", enabled: true},
		&fakeProbe{name: "memory", enabled: true},
	)
	got := m.enabledCapabilityIDs()
	want := []string{"llm_inference", "silo_recall", "silo_remember"}
	if len(got) != len(want) {
		t.Fatalf("ids = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestStepIdleWhenNothingEnabled(t *testing.T) {
	m := newProbeManager(&fakeProbe{name: "compute", enabled: false})
	m.SetTunnelURL("https://abc.trycloudflare.com")
	if wait := m.step(context.Background()); wait != idleInterval {
		t.Errorf("wait = %v, want idle when no capability is enabled", wait)
	}
}

func TestStepIdleWithoutTunnelURL(t *testing.T) {
	m := newProbeManager(&fakeProbe{name: "compute", enabled: true, healthy: true})
	if wait := m.step(context.Background()); wait != idleInterval {
		t.Errorf("wait = %v, want idle without a tunnel URL", wait)
	}
}
