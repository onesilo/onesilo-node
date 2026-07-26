package pairing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func jwksDoc(t *testing.T, kid string, pub *ecdsa.PublicKey) string {
	t.Helper()
	x := base64.RawURLEncoding.EncodeToString(pub.X.FillBytes(make([]byte, 32)))
	y := base64.RawURLEncoding.EncodeToString(pub.Y.FillBytes(make([]byte, 32)))
	return fmt.Sprintf(`{"keys":[{"kty":"EC","crv":"P-256","kid":%q,"alg":"ES256","x":%q,"y":%q}]}`, kid, x, y)
}

func TestHTTPKeySourceFetchAndCache(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != jwksPath {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		fmt.Fprint(w, jwksDoc(t, "silo-assert-1", &key.PublicKey))
	}))
	defer srv.Close()

	ks := NewHTTPKeySource(func() string { return srv.URL })
	now := time.Unix(1_700_000_000, 0)
	ks.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		keys, err := ks.Keys(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if keys["silo-assert-1"] == nil {
			t.Fatal("expected assertion key in fetched JWKS")
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("expected a single fetch (cache hits after), got %d", hits.Load())
	}

	// After the TTL elapses the source refetches.
	now = now.Add(jwksCacheTTL + time.Second)
	if _, err := ks.Keys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected a refetch after TTL, got %d fetches", hits.Load())
	}
}

func TestHTTPKeySourceServesStaleOnError(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, jwksDoc(t, "silo-assert-1", &key.PublicKey))
	}))
	defer srv.Close()

	ks := NewHTTPKeySource(func() string { return srv.URL })
	now := time.Unix(1_700_000_000, 0)
	ks.now = func() time.Time { return now }

	if _, err := ks.Keys(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Control plane goes down and the cache expires: a *usable* stale cache is
	// served rather than failing pairing for an already-known key.
	fail.Store(true)
	now = now.Add(jwksCacheTTL + time.Second)
	keys, err := ks.Keys(context.Background())
	if err != nil {
		t.Fatalf("expected stale cache to be served on outage, got %v", err)
	}
	if keys["silo-assert-1"] == nil {
		t.Fatal("stale cache lost the key")
	}
}

func TestHTTPKeySourceFailsClosedWithNoCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ks := NewHTTPKeySource(func() string { return srv.URL })
	if _, err := ks.Keys(context.Background()); err == nil {
		t.Fatal("expected an error when the first fetch fails and there is no cache")
	}
}

func TestHTTPKeySourceCoalescesConcurrentFetches(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var hits atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release // hold the leader's request open while waiters pile up
		fmt.Fprint(w, jwksDoc(t, "silo-assert-1", &key.PublicKey))
	}))
	defer srv.Close()

	ks := NewHTTPKeySource(func() string { return srv.URL })

	// Leader goroutine enters the (blocked) handler.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); ks.Keys(context.Background()) }()
	<-entered // inflight is now set; the handler is parked on <-release

	// A burst of waiters must share the in-flight fetch, not start their own.
	const waiters = 16
	var started atomic.Int32
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started.Add(1)
			if _, err := ks.Keys(context.Background()); err != nil {
				t.Errorf("waiter got error: %v", err)
			}
		}()
	}
	for started.Load() < waiters {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let waiters reach the inflight wait
	close(release)
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Fatalf("expected a single coalesced fetch, got %d", got)
	}
}

func TestHTTPKeySourceKidRefresh(t *testing.T) {
	// The endpoint rotates its kid on the second fetch; a token carrying the
	// new kid triggers a single refresh via KeysForKid.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var kid atomic.Value
	kid.Store("kid-old")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, jwksDoc(t, kid.Load().(string), &key.PublicKey))
	}))
	defer srv.Close()

	ks := NewHTTPKeySource(func() string { return srv.URL })
	base := time.Unix(1_700_000_000, 0)
	ks.now = func() time.Time { return base }

	if _, err := ks.Keys(context.Background()); err != nil {
		t.Fatal(err)
	}
	kid.Store("kid-new")
	// Advance past the min-refresh window so the kid miss is allowed to fetch.
	ks.now = func() time.Time { return base.Add(jwksMinRefresh + time.Second) }
	keys, err := ks.KeysForKid(context.Background(), "kid-new")
	if err != nil {
		t.Fatal(err)
	}
	if keys["kid-new"] == nil {
		t.Fatal("kid refresh did not pick up the rotated key")
	}
}
