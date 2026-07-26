package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"
)

func TestBackoffForClassifiesErrors(t *testing.T) {
	transient := 2 * time.Second
	cases := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"subscription", fmt.Errorf("wrap: %w", ErrSubscriptionRequired), subscriptionBackoff},
		{"unavailable", fmt.Errorf("wrap: %w", ErrManagedUnavailable), unavailableBackoff},
		{"transient", errors.New("connection refused"), transient},
	}
	for _, tc := range cases {
		if got := backoffFor(tc.err, transient); got != tc.want {
			t.Errorf("%s: backoffFor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReadyScanWriterDetectsMarkerAcrossWrites(t *testing.T) {
	ready := make(chan struct{}, 1)
	w := &readyScanWriter{ready: ready, logger: slog.Default()}

	// The marker split across two writes must still be detected once the
	// line completes.
	w.Write([]byte("2026-07-26 INF Registered tunnel conn"))
	select {
	case <-ready:
		t.Fatal("signalled before the line completed")
	default:
	}
	w.Write([]byte("ection connIndex=0 location=sea01\n"))
	select {
	case <-ready:
	default:
		t.Fatal("marker line not detected")
	}

	// Later output is discarded without re-signalling.
	w.Write([]byte("INF Registered tunnel connection connIndex=1\n"))
	select {
	case <-ready:
		t.Fatal("signalled twice")
	default:
	}
}

func TestReadyScanWriterIgnoresOtherLines(t *testing.T) {
	ready := make(chan struct{}, 1)
	w := &readyScanWriter{ready: ready, logger: slog.Default()}
	w.Write([]byte("INF Starting tunnel\nINF Version 2026.1.0\n"))
	select {
	case <-ready:
		t.Fatal("signalled on non-marker output")
	default:
	}
}

func TestManagedManagerSubscriptionBlockedFlag(t *testing.T) {
	m := NewManaged(func(ctx context.Context) (Provisioned, error) {
		return Provisioned{}, fmt.Errorf("wrap: %w", ErrSubscriptionRequired)
	}, "", slog.Default(), nil)

	if m.SubscriptionBlocked() {
		t.Fatal("blocked before any provisioning attempt")
	}
	m.setSubscriptionBlocked(true)
	if !m.SubscriptionBlocked() {
		t.Fatal("flag not set")
	}
	m.setSubscriptionBlocked(false)
	if m.SubscriptionBlocked() {
		t.Fatal("flag not cleared")
	}
}

func TestManagedStartFailsWithoutBinary(t *testing.T) {
	m := NewManaged(func(ctx context.Context) (Provisioned, error) {
		return Provisioned{URL: "https://x.tunnel.onesilo.com", Token: "tok"}, nil
	}, "/nonexistent/cloudflared", slog.Default(), nil)
	if err := m.Start(context.Background()); err == nil {
		m.Stop()
		t.Fatal("Start succeeded with a bogus cloudflared path")
	}
}
