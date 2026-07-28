package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/onesilo/onesilo-node/internal/adminapi"
)

// adminTokenFile is the admin API token's location inside data_dir. Written
// by `silo-node setup` and loaded automatically at start when
// SILO_NODE_ADMIN_TOKEN is unset.
const adminTokenFile = "admin.token"

// ensureAdminToken returns the node's persistent admin token, generating
// <dataDir>/admin.token (0600) on first run.
func ensureAdminToken(dataDir string) (token string, created bool, err error) {
	path := filepath.Join(dataDir, adminTokenFile)
	if b, readErr := os.ReadFile(path); readErr == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, false, nil
		}
	}
	// 0700: this directory holds admin.token, node.key, memory.key,
	// oauth.json, pairing.key — every node secret. setup runs this before
	// the node's own 0700 MkdirAll, and MkdirAll won't tighten an existing
	// dir, so it must be created restrictively here.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", false, fmt.Errorf("creating data dir: %w", err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("generating admin token: %w", err)
	}
	token = hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("writing %s: %w", path, err)
	}
	return token, true, nil
}

// resolveAdminToken returns the admin token and where it came from:
// the SILO_NODE_ADMIN_TOKEN env var when set ("env"), else
// <dataDir>/admin.token ("file"), else "" / "".
func resolveAdminToken(dataDir string, lookup func(string) (string, bool)) (token, source string) {
	if v, ok := lookup(adminapi.AdminTokenEnvVar); ok && v != "" {
		return v, "env"
	}
	if dataDir == "" {
		return "", ""
	}
	b, err := os.ReadFile(filepath.Join(dataDir, adminTokenFile))
	if err != nil {
		return "", ""
	}
	if t := strings.TrimSpace(string(b)); t != "" {
		return t, "file"
	}
	return "", ""
}
