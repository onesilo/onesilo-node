package lanserve

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"

	"github.com/onesilo/silo-node/internal/pairing"
)

// Pairer holds the node-wide state for automated device pairing: how to
// build a per-connection handshake responder, the TOFU pin store, whether
// first-contact SAS verification is required before traffic flows, and the
// registry of pending SAS codes the admin surface exposes for verification.
//
// A nil *Pairer disables automated pairing entirely (the router then only
// serves the legacy file pairing.key path) — this keeps existing tests and
// memory-only deployments working unchanged.
type Pairer struct {
	// newResponder builds a fresh responder per connection (own identity key
	// + assertion verifier bound to this node/account). Never reused.
	newResponder func() *pairing.Responder
	pins         *pairing.PinStore
	// requireVerify hard-gates first-contact sessions: a newly pinned app key
	// cannot run inference until its SAS is confirmed via the admin API. This
	// is the safer default — an active control plane that substituted keys
	// would produce a mismatching SAS and be caught before any real traffic.
	requireVerify bool
	logger        *slog.Logger

	mu      sync.Mutex
	pending map[string]PendingPairing // key: account_id + "\x00" + app_id_pub
}

// PendingPairing is one first-contact pairing awaiting SAS confirmation.
type PendingPairing struct {
	AccountID   string `json:"account_id"`
	AppIDPubB64 string `json:"app_id_pub"`
	SAS         string `json:"sas"`
}

// NewPairer builds a Pairer. newResponder and pins must be non-nil.
func NewPairer(newResponder func() *pairing.Responder, pins *pairing.PinStore, requireVerify bool, logger *slog.Logger) *Pairer {
	return &Pairer{
		newResponder:  newResponder,
		pins:          pins,
		requireVerify: requireVerify,
		logger:        logger,
		pending:       map[string]PendingPairing{},
	}
}

func pendingKey(account, appIDPub string) string { return account + "\x00" + appIDPub }

// recordPending registers a first-contact SAS for admin-side verification.
func (p *Pairer) recordPending(pp PendingPairing) {
	p.mu.Lock()
	p.pending[pendingKey(pp.AccountID, pp.AppIDPubB64)] = pp
	p.mu.Unlock()
}

// clearPending removes a pairing from the pending list once verified.
func (p *Pairer) clearPending(account, appIDPub string) {
	p.mu.Lock()
	delete(p.pending, pendingKey(account, appIDPub))
	p.mu.Unlock()
}

// Pending returns the pairings awaiting SAS confirmation (for the admin UI).
func (p *Pairer) Pending() []PendingPairing {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PendingPairing, 0, len(p.pending))
	for _, pp := range p.pending {
		out = append(out, pp)
	}
	return out
}

// Verify confirms a pending pairing's SAS (operator matched the code shown in
// the app). It pins the key as verified so this and future connections from
// it are trusted. Returns an error if there is no such pending pairing.
func (p *Pairer) Verify(accountID, appIDPubB64 string) error {
	if p == nil {
		return errors.New("automated pairing is not enabled on this node")
	}
	if err := p.pins.Verify(accountID, appIDPubB64); err != nil {
		return err
	}
	p.clearPending(accountID, appIDPubB64)
	if p.logger != nil {
		p.logger.Info("pairing SAS verified", "account_id", accountID)
	}
	return nil
}

// --- per-connection handshake handling ---

// handleHello processes a plaintext pair_hello frame: runs the authenticated
// ECDH, replies with pair_ack, and records the SAS. It does not yet enable
// the session key — that waits for a matching pair_confirm.
func (r *Router) handleHello(ctx context.Context, s *Session, frame []byte) {
	if r.pairer == nil {
		s.sendJSON(ctx, pairing.PairError{Type: pairing.FrameError, Error: "automated pairing is not enabled on this node"})
		return
	}
	var h pairing.Hello
	if err := json.Unmarshal(frame, &h); err != nil {
		s.sendJSON(ctx, pairing.PairError{Type: pairing.FrameError, Error: "malformed pair_hello"})
		return
	}

	s.pairMu.Lock()
	if s.pairResponder == nil {
		s.pairResponder = r.pairer.newResponder()
	}
	s.pairStarted = true // no-downgrade: this connection is now handshake-only
	responder := s.pairResponder
	s.pairMu.Unlock()

	ack, err := responder.Hello(ctx, &h)
	if err != nil {
		r.logger.Warn("pair_hello rejected", "error", err)
		s.sendJSON(ctx, pairing.PairError{Type: pairing.FrameError, Error: err.Error()})
		return
	}
	s.sendJSON(ctx, ack)
}

// handleConfirm processes a plaintext pair_confirm: verifies the app's key
// confirmation, applies TOFU pinning, and either activates the session key
// (known-verified key, or verification not required) or leaves the session
// pending SAS confirmation (first contact under the safer default).
func (r *Router) handleConfirm(ctx context.Context, s *Session, frame []byte) {
	if r.pairer == nil {
		s.sendJSON(ctx, pairing.PairError{Type: pairing.FrameError, Error: "automated pairing is not enabled on this node"})
		return
	}
	var c pairing.Confirm
	if err := json.Unmarshal(frame, &c); err != nil {
		s.sendJSON(ctx, pairing.PairError{Type: pairing.FrameError, Error: "malformed pair_confirm"})
		return
	}

	s.pairMu.Lock()
	responder := s.pairResponder
	s.pairMu.Unlock()
	if responder == nil {
		s.sendJSON(ctx, pairing.PairError{Type: pairing.FrameError, Error: "pair_confirm before pair_hello"})
		return
	}

	res, err := responder.Confirm(&c)
	if err != nil {
		r.logger.Warn("pair_confirm rejected", "error", err)
		s.sendJSON(ctx, pairing.PairError{Type: pairing.FrameError, Error: err.Error()})
		return
	}

	// TOFU: record/observe the app key. A changed key for an account with a
	// previously verified key is a hard stop (safety number changed).
	status, err := r.pairer.pins.Observe(res.AccountID, res.AppIDPubB64)
	if err != nil {
		r.logger.Warn("pairing pin rejected", "error", err, "account_id", res.AccountID)
		s.sendJSON(ctx, pairing.PairError{Type: pairing.FrameError, Error: err.Error()})
		return
	}

	verified := status == pairing.PinKnownVerified || !r.pairer.requireVerify

	s.pairMu.Lock()
	s.sessionKey = res.SessionKey
	s.pairVerified = verified
	s.pairAccountID = res.AccountID
	s.pairAppIDPub = res.AppIDPubB64
	s.pairMu.Unlock()

	if verified {
		r.logger.Info("pairing established", "account_id", res.AccountID, "verified", true)
		s.sendJSON(ctx, pairResult{Type: "pair_result", Verified: true})
		return
	}

	// First contact with verification required: surface the SAS and hold the
	// session until the operator confirms it out of band.
	r.pairer.recordPending(PendingPairing{AccountID: res.AccountID, AppIDPubB64: res.AppIDPubB64, SAS: res.SAS})
	r.logger.Warn("pairing awaiting SAS verification",
		"account_id", res.AccountID, "sas", res.SAS,
		"hint", "compare this code with the app, then confirm in the node admin UI")
	s.sendJSON(ctx, pairResult{Type: "pair_result", Verified: false, SAS: res.SAS})
}

// sessionInferenceAllowed reports whether user_message traffic may run on
// this session. A legacy (non-handshake) session is always allowed. A
// handshake session is allowed once verified; if it is still pending, the pin
// store is re-checked so an operator confirmation that landed after the
// handshake (via the admin API) is picked up without reconnecting.
func (r *Router) sessionInferenceAllowed(s *Session) bool {
	s.pairMu.Lock()
	started := s.pairStarted
	verified := s.pairVerified
	account := s.pairAccountID
	appIDPub := s.pairAppIDPub
	s.pairMu.Unlock()

	if !started || verified {
		return true
	}
	if r.pairer != nil && r.pairer.pins.IsVerified(account, appIDPub) {
		s.pairMu.Lock()
		s.pairVerified = true
		s.pairMu.Unlock()
		return true
	}
	return false
}

// pairResult tells the app whether the handshake left the session ready
// (verified) or pending SAS confirmation. SAS is included when pending so the
// app can display the same code for comparison.
type pairResult struct {
	Type     string `json:"type"`
	Verified bool   `json:"verified"`
	SAS      string `json:"sas,omitempty"`
}
