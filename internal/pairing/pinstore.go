package pairing

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/onesilo/onesilo-node/internal/fsutil"
)

// pinFile stores the trust-on-first-use record of app identity keys this
// node has paired with, per account.
const pinFile = "paired_devices.json"

// ErrSafetyNumberChanged means a previously pinned app identity key changed
// for the same account: the classic "safety number changed" event. It is a
// hard stop — the operator must re-verify (the SAS) before the new key is
// trusted, exactly as Signal/WhatsApp treat a changed safety number.
var ErrSafetyNumberChanged = errors.New("app identity key changed since last pairing (re-verification required)")

// pinnedDevice is one recorded pairing.
type pinnedDevice struct {
	AccountID   string    `json:"account_id"`
	AppIDPubB64 string    `json:"app_id_pub"`
	Verified    bool      `json:"verified"` // SAS confirmed by the operator
	FirstSeen   time.Time `json:"first_seen"`
	VerifiedAt  time.Time `json:"verified_at,omitempty"`
}

// PinStore is the node's TOFU registry of app identity keys. It persists to
// <data_dir>/paired_devices.json (0600) and is safe for concurrent use.
type PinStore struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	devices map[string]pinnedDevice // key: account_id + "\x00" + app_id_pub
}

// LoadPinStore opens (creating on first write) the pin store under dataDir.
func LoadPinStore(dataDir string) (*PinStore, error) {
	s := &PinStore{
		path:    filepath.Join(dataDir, pinFile),
		now:     time.Now,
		devices: map[string]pinnedDevice{},
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("reading pin store: %w", err)
	}
	var list []pinnedDevice
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("pin store at %s is corrupt: %w", s.path, err)
	}
	for _, d := range list {
		s.devices[pinKey(d.AccountID, d.AppIDPubB64)] = d
	}
	return s, nil
}

func pinKey(account, appIDPub string) string { return account + "\x00" + appIDPub }

// PinStatus is the outcome of observing an app key during a handshake.
type PinStatus int

const (
	// PinFirstContact: no key was pinned for this account before — this is a
	// brand-new pairing that needs SAS verification.
	PinFirstContact PinStatus = iota
	// PinKnownVerified: this exact key was pinned and SAS-verified before.
	PinKnownVerified
	// PinKnownUnverified: this exact key was seen before but never verified
	// (a prior first-contact that wasn't confirmed).
	PinKnownUnverified
)

// Observe records an app identity key seen during a handshake for account,
// and reports how it relates to what's already pinned:
//
//   - The same key seen before → PinKnownVerified / PinKnownUnverified.
//   - A different key while the account already has a *verified* key →
//     ErrSafetyNumberChanged (a hard stop; the operator must re-verify the new
//     key with Verify, which then supersedes the old one).
//   - Otherwise → PinFirstContact, recorded unverified. Any prior *unverified*
//     keys for the same account are evicted first, so at most one pending
//     pairing per account is retained (the newest attempt supersedes stale
//     ones and the store can't grow unbounded from repeated first contacts).
func (s *PinStore) Observe(accountID, appIDPubB64 string) (PinStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if d, ok := s.devices[pinKey(accountID, appIDPubB64)]; ok {
		if d.Verified {
			return PinKnownVerified, nil
		}
		return PinKnownUnverified, nil
	}
	// No record for this exact key. Is there a *different* verified key for
	// the same account? That is a safety-number change, not a first contact.
	for _, d := range s.devices {
		if d.AccountID == accountID && d.AppIDPubB64 != appIDPubB64 && d.Verified {
			return PinFirstContact, ErrSafetyNumberChanged
		}
	}
	// First contact. Evict any stale *unverified* keys for this account (only
	// one pending pairing is meaningful at a time); verified keys, if any,
	// were already handled by the safety-number check above.
	for k, d := range s.devices {
		if d.AccountID == accountID && !d.Verified {
			delete(s.devices, k)
		}
	}
	s.devices[pinKey(accountID, appIDPubB64)] = pinnedDevice{
		AccountID:   accountID,
		AppIDPubB64: appIDPubB64,
		Verified:    false,
		FirstSeen:   s.now(),
	}
	if err := s.saveLocked(); err != nil {
		return PinFirstContact, err
	}
	return PinFirstContact, nil
}

// Verify marks a first-contact key as SAS-verified (operator confirmed the
// short authentication string). Returns an error if the key was never
// observed.
func (s *PinStore) Verify(accountID, appIDPubB64 string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := pinKey(accountID, appIDPubB64)
	d, ok := s.devices[k]
	if !ok {
		return fmt.Errorf("no pending pairing for this app key")
	}
	d.Verified = true
	d.VerifiedAt = s.now()
	s.devices[k] = d
	return s.saveLocked()
}

// IsVerified reports whether the given app key is pinned and SAS-verified.
func (s *PinStore) IsVerified(accountID, appIDPubB64 string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[pinKey(accountID, appIDPubB64)]
	return ok && d.Verified
}

func (s *PinStore) saveLocked() error {
	list := make([]pinnedDevice, 0, len(s.devices))
	for _, d := range s.devices {
		list = append(list, d)
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.path, raw, 0o600)
}
