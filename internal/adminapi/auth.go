package adminapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// AdminTokenEnvVar is where the desktop app (or operator) provides the
// shared secret for the admin API.
const AdminTokenEnvVar = "ONESILO_NODE_ADMIN_TOKEN"

// requireAdminToken wraps a handler with a constant-time bearer check
// against the configured admin token. /healthz is registered outside this
// wrapper and stays unauthenticated.
//
// Fail closed: with no token configured, every authenticated route is
// refused — an unconfigured node must not expose a writable control
// surface, even on localhost.
func requireAdminToken(token string, next http.Handler) http.Handler {
	// Hash both sides so the comparison is constant-time regardless of
	// length differences.
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			writeError(w, http.StatusUnauthorized,
				"admin API disabled: "+AdminTokenEnvVar+" is not set")
			return
		}
		auth := r.Header.Get("Authorization")
		bearer, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		got := sha256.Sum256([]byte(bearer))
		if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
