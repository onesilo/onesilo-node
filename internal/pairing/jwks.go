package pairing

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// jwksPath is the control plane's JWKS surface (the OAuth server publishes
// both the RS256 OAuth key and the ES256 pairing-assertion key here).
const jwksPath = "/oauth/jwks"

// jwksCacheTTL bounds how long a fetched key set is trusted before a
// background-eligible refresh. Assertions are short-lived (≤60s) and signed
// by a stable key, so a few minutes of caching is safe and keeps the node
// from fetching JWKS on every pairing.
const jwksCacheTTL = 10 * time.Minute

// jwksMinRefresh rate-limits refreshes triggered by an unknown kid, so a
// stream of tokens carrying a bogus kid cannot turn into a fetch storm
// against the control plane.
const jwksMinRefresh = 30 * time.Second

// HTTPKeySource is a KeySource backed by the control plane's JWKS endpoint.
// It caches the parsed ES256 keys and refreshes them lazily: on expiry, or
// once (rate-limited) when a token references a kid not in the cache — so a
// key rotation is picked up without a restart, while a bad kid can't be used
// to hammer the control plane. Safe for concurrent use.
type HTTPKeySource struct {
	baseURL func() string // control plane base URL, resolved per fetch
	http    *http.Client
	now     func() time.Time

	mu          sync.Mutex
	keys        ECPublicKeys
	fetchedAt   time.Time
	lastAttempt time.Time
	// inflight coalesces concurrent refreshes: while one fetch is running,
	// other callers wait on it and share its result instead of each firing
	// their own request (no thundering herd on TTL expiry or a kid-miss burst).
	inflight *fetchCall
}

// fetchCall is one in-flight JWKS fetch that concurrent callers share.
type fetchCall struct {
	done chan struct{}
	keys ECPublicKeys
	err  error
}

// NewHTTPKeySource builds a JWKS-backed key source. baseURL is resolved on
// each fetch so a reconfigured control-plane URL is honored.
func NewHTTPKeySource(baseURL func() string) *HTTPKeySource {
	return &HTTPKeySource{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 10 * time.Second},
		now:     time.Now,
	}
}

// Keys implements KeySource. It returns the cached key set when fresh,
// otherwise fetches once. A fetch error with a usable (merely stale) cache
// returns the stale cache rather than failing closed — a transient control
// plane outage must not break pairing for an already-known signing key; a
// fetch error with no cache at all fails closed.
func (s *HTTPKeySource) Keys(ctx context.Context) (ECPublicKeys, error) {
	s.mu.Lock()
	fresh := s.keys != nil && s.now().Sub(s.fetchedAt) < jwksCacheTTL
	cached := s.keys
	s.mu.Unlock()
	if fresh {
		return cached, nil
	}
	keys, err := s.refresh(ctx)
	if err != nil {
		if cached != nil {
			return cached, nil // serve stale rather than fail an outage
		}
		return nil, err
	}
	return keys, nil
}

// KeysForKid implements KidRefresher: it forces a single rate-limited
// refresh when the verifier has a token whose kid is missing from the cache
// (likely a rotation). It returns the freshest key set it can, without
// failing closed on the rate limit.
func (s *HTTPKeySource) KeysForKid(ctx context.Context, kid string) (ECPublicKeys, error) {
	s.mu.Lock()
	if s.keys != nil {
		if _, ok := s.keys[kid]; ok {
			keys := s.keys
			s.mu.Unlock()
			return keys, nil
		}
	}
	if s.now().Sub(s.lastAttempt) < jwksMinRefresh {
		keys := s.keys
		s.mu.Unlock()
		if keys != nil {
			return keys, nil
		}
		return nil, fmt.Errorf("no trusted assertion key for kid %q (JWKS refresh rate-limited)", kid)
	}
	s.mu.Unlock()
	return s.refresh(ctx)
}

// refresh fetches the JWKS, coalescing concurrent calls: the first caller
// becomes the leader and performs the single network request; others wait on
// the same fetchCall and share its result (or bail if their own ctx is
// cancelled first). On success the cache and fetch time are updated.
func (s *HTTPKeySource) refresh(ctx context.Context) (ECPublicKeys, error) {
	s.mu.Lock()
	if call := s.inflight; call != nil {
		s.mu.Unlock()
		select {
		case <-call.done:
			return call.keys, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &fetchCall{done: make(chan struct{})}
	s.inflight = call
	s.lastAttempt = s.now()
	s.mu.Unlock()

	keys, err := s.fetch(ctx)

	s.mu.Lock()
	if err == nil {
		s.keys = keys
		s.fetchedAt = s.now()
	}
	s.inflight = nil
	s.mu.Unlock()

	call.keys, call.err = keys, err
	close(call.done)
	return keys, err
}

// fetch performs one JWKS GET and parse with no shared-state mutation.
func (s *HTTPKeySource) fetch(ctx context.Context) (ECPublicKeys, error) {
	base := strings.TrimRight(s.baseURL(), "/")
	if base == "" {
		return nil, fmt.Errorf("control plane URL is not configured; cannot fetch assertion keys")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+jwksPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching JWKS from %s: %w", base+jwksPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching JWKS: control plane returned %d", resp.StatusCode)
	}
	// Bound the body: a JWKS document is a few keys, never megabytes.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, fmt.Errorf("reading JWKS: %w", err)
	}
	return ParseJWKS(body)
}
