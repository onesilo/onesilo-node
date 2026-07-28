// Package version carries build-time identity injected via -ldflags:
//
//	go build -ldflags "-X github.com/onesilo/onesilo-node/internal/version.Version=v0.1.0 \
//	                   -X github.com/onesilo/onesilo-node/internal/version.Commit=abc1234"
package version

var (
	// Version is the semantic version of this build ("dev" for local builds).
	Version = "dev"
	// Commit is the short git commit hash of this build.
	Commit = "none"
)

// UserAgent returns the User-Agent string used for control-plane requests.
func UserAgent() string {
	return "onesilo-node/" + Version
}
