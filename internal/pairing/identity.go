package pairing

import (
	"crypto/ecdh"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/onesilo/silo-node/internal/fsutil"
)

// identityKeyFile holds the node's long-term P-256 identity private key.
const identityKeyFile = "device_identity.key"

// LoadOrCreateIdentity returns the node's long-term identity keypair from
// <dataDir>/device_identity.key, generating it (0600) on first call. This is
// the node's stable cryptographic identity for pairing — the public key is
// published to the control plane; the private key never leaves the host.
//
// The private key is a genuine secret stored 0600 like node.key / memory.key.
// Forward secrecy (the ephemeral-ephemeral DH layer) is what protects
// recorded sessions if this file is later stolen; on platforms with a
// hardware keystore (e.g. macOS Keychain when bundled in SiloDesktop) the
// stored bytes should additionally be wrapped — tracked as a follow-up.
func LoadOrCreateIdentity(dataDir string) (*ecdh.PrivateKey, error) {
	path := filepath.Join(dataDir, identityKeyFile)
	raw, err := os.ReadFile(path)
	if err == nil {
		key, decErr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil {
			return nil, fmt.Errorf("identity key at %s is not valid hex", path)
		}
		priv, keyErr := Curve().NewPrivateKey(key)
		if keyErr != nil {
			return nil, fmt.Errorf("identity key at %s is malformed: %w", path, keyErr)
		}
		return priv, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading identity key: %w", err)
	}

	priv, err := GenerateEphemeral() // same curve; generates a fresh P-256 key
	if err != nil {
		return nil, fmt.Errorf("generating identity key: %w", err)
	}
	if err := fsutil.WriteFileAtomic(path, []byte(hex.EncodeToString(priv.Bytes())), 0o600); err != nil {
		return nil, fmt.Errorf("writing identity key: %w", err)
	}
	return priv, nil
}
