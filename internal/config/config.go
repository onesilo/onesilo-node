// Package config defines the typed silo-node configuration and its
// load/save machinery. Precedence: flags > SILO_NODE_* env > TOML file >
// defaults (see load.go).
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Node modes. Mode selects what the node *relays*; whether it is reachable
// from the control plane is the orthogonal "exposure" axis (see Exposed).
const (
	// ModeLocal ("Local Node") serves local memory and local LLM only and
	// does not relay the control plane's cloud surface. It may still be
	// exposed (tunnel + destination registration) so its local compute and
	// memory are reachable from the owner's authenticated apps.
	ModeLocal = "local"
	// ModeGateway ("Local Relay") additionally relays the control plane's
	// cloud surface (cloud silos, connectors, MCP) to local clients via
	// /v1/cloud, using its own credentials.
	ModeGateway = "gateway"
)

// Tunnel modes. A non-"off" tunnel makes the node reachable from the
// control plane (see Exposed) — available in either node mode.
const (
	TunnelModeOff = "off"
	// TunnelModeManaged is the preferred remote-access mode: the control
	// plane provisions a named Cloudflare tunnel with a stable hostname
	// (<slug>.tunnel.onesilo.com) and the node runs cloudflared with a
	// per-start token fetched from the control plane. Paid feature.
	TunnelModeManaged = "managed"
	// TunnelModeQuick mints an ephemeral *.trycloudflare.com quick tunnel.
	// Kept for dev and as a fallback when managed provisioning is
	// unavailable; the hostname changes on every cloudflared restart.
	TunnelModeQuick    = "quick"
	TunnelModeExternal = "external"
)

// Auth modes for the control plane.
const (
	AuthModeJWT    = "jwt"
	AuthModeAPIKey = "api_key"
	// AuthModeOAuth uses the credential stored by the `silo-node setup`
	// sign-in step (<data_dir>/oauth.json): the node holds its own OAuth
	// grant — like the Silo iOS app — and appears as a connection in the
	// owner's dashboard. Tokens refresh automatically.
	AuthModeOAuth = "oauth"
)

// Config is the full silo-node configuration.
type Config struct {
	// Mode selects what this node is: "local" (self-contained, never talks
	// to the control plane) or "gateway" (control-plane relay). See the
	// mode constants above.
	Mode string `toml:"mode" json:"mode"`

	// DataDir holds node state (device_id, pairing.key, persisted config).
	// A leading "~" is expanded; use ResolvedDataDir for the absolute path.
	DataDir string `toml:"data_dir" json:"data_dir"`

	Log          Log          `toml:"log" json:"log"`
	Capabilities Capabilities `toml:"capabilities" json:"capabilities"`
	ControlPlane ControlPlane `toml:"control_plane" json:"control_plane"`
	Memory       Memory       `toml:"memory" json:"memory"`
	Ollama       Ollama       `toml:"ollama" json:"ollama"`
	Tunnel       Tunnel       `toml:"tunnel" json:"tunnel"`
	LAN          LAN          `toml:"lan" json:"lan"`
	Admin        Admin        `toml:"admin" json:"admin"`
}

// Log configures slog output.
type Log struct {
	Format string `toml:"format" json:"format"` // "text" | "json"
	Level  string `toml:"level" json:"level"`   // "debug" | "info" | "warn" | "error"
}

// Capabilities toggles what this node contributes to the control plane.
type Capabilities struct {
	Memory  bool `toml:"memory" json:"memory"`
	Compute bool `toml:"compute" json:"compute"`
}

// ControlPlane configures registration with the Silo backend.
type ControlPlane struct {
	URL      string `toml:"url" json:"url"`
	AuthMode string `toml:"auth_mode" json:"auth_mode"` // "jwt" | "api_key"
	// DeviceName defaults to the OS hostname when empty.
	DeviceName string `toml:"device_name" json:"device_name"`
}

// Memory configures the memory capability (capabilities.memory toggles it).
type Memory struct {
	// EmbedModel is the Ollama embedding model used for hybrid recall when
	// the compute capability is available. Empty means nomic-embed-text.
	EmbedModel string `toml:"embed_model" json:"embed_model"`
}

// Ollama configures the local inference runtime for the compute capability.
type Ollama struct {
	Host string `toml:"host" json:"host"`
	// Manage spawns `ollama serve` when no server answers at Host.
	Manage       bool   `toml:"manage" json:"manage"`
	DefaultModel string `toml:"default_model" json:"default_model"`
	// BinaryPath optionally pins the ollama binary used when Manage is true.
	BinaryPath string `toml:"binary_path" json:"binary_path"`
}

// Tunnel configures how the node becomes reachable from outside the LAN.
type Tunnel struct {
	Mode string `toml:"mode" json:"mode"` // "off" | "quick" | "external"
	// CloudflaredPath optionally pins the cloudflared binary (quick mode).
	CloudflaredPath string `toml:"cloudflared_path" json:"cloudflared_path"`
	// ExternalURL is the public https URL when Mode is "external".
	ExternalURL string `toml:"external_url" json:"external_url"`
}

// LAN configures local-network serving. Stub: the LAN server arrives in a
// later phase; the quick tunnel already points at this port.
type LAN struct {
	Enabled bool `toml:"enabled" json:"enabled"`
	Port    int  `toml:"port" json:"port"`
	// RequirePairingVerification hard-gates automated pairing: a first-contact
	// app identity key cannot run inference until its short authentication
	// string (SAS) is confirmed in the admin UI. Default true (the safer
	// posture — it catches an active control plane that substituted keys).
	// Set false to trust-on-first-use without the manual SAS comparison.
	RequirePairingVerification bool `toml:"require_pairing_verification" json:"require_pairing_verification"`
}

// Admin configures the localhost-only admin API.
type Admin struct {
	Port int `toml:"port" json:"port"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Mode:    ModeLocal,
		DataDir: "~/.silo-node",
		Log: Log{
			Format: "text",
			Level:  "info",
		},
		Capabilities: Capabilities{
			Memory:  false,
			Compute: false,
		},
		ControlPlane: ControlPlane{
			URL:      "https://api.onesilo.com",
			AuthMode: AuthModeJWT,
		},
		Memory: Memory{
			EmbedModel: "nomic-embed-text",
		},
		Ollama: Ollama{
			Host:         "http://127.0.0.1:11434",
			Manage:       false,
			DefaultModel: "llama3.2:3b",
		},
		Tunnel: Tunnel{
			Mode: TunnelModeOff,
		},
		LAN: LAN{
			Enabled:                    false,
			Port:                       8765,
			RequirePairingVerification: true,
		},
		Admin: Admin{
			Port: 8766,
		},
	}
}

// Exposed reports whether the node is reachable from the control plane —
// i.e. a tunnel is configured (a managed named tunnel, a quick tunnel, or a
// bring-your-own external URL). An exposed node registers itself as a
// destination so the owner's authenticated apps can reach its local
// compute/memory from
// anywhere. Orthogonal to Mode: a Local Node can be exposed, and a Local
// Relay can be kept LAN-only.
func (c *Config) Exposed() bool {
	return c.Tunnel.Mode != TunnelModeOff
}

// Validate checks enum fields, port ranges, and mode consistency.
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeLocal, ModeGateway:
	default:
		return fmt.Errorf("mode must be %q or %q, got %q", ModeLocal, ModeGateway, c.Mode)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("log.format must be \"text\" or \"json\", got %q", c.Log.Format)
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level must be one of debug|info|warn|error, got %q", c.Log.Level)
	}
	switch c.ControlPlane.AuthMode {
	case AuthModeJWT, AuthModeAPIKey, AuthModeOAuth:
	default:
		return fmt.Errorf("control_plane.auth_mode must be one of %q, %q or %q, got %q",
			AuthModeJWT, AuthModeAPIKey, AuthModeOAuth, c.ControlPlane.AuthMode)
	}
	// Validate the tunnel fields first: Exposed() is derived from tunnel.mode,
	// so an invalid tunnel.mode must surface as such rather than as a
	// downstream control-plane URL error.
	switch c.Tunnel.Mode {
	case TunnelModeOff, TunnelModeManaged, TunnelModeQuick, TunnelModeExternal:
	default:
		return fmt.Errorf("tunnel.mode must be one of off|managed|quick|external, got %q", c.Tunnel.Mode)
	}
	if c.Tunnel.Mode == TunnelModeExternal {
		if !strings.HasPrefix(c.Tunnel.ExternalURL, "https://") {
			return fmt.Errorf("tunnel.external_url must be an https:// URL when tunnel.mode is %q", TunnelModeExternal)
		}
	}
	// A node that contacts the control plane — a relay (gateway mode) or an
	// exposed node registering itself as a destination — sends OAuth codes,
	// refresh tokens, and bearer JWTs there, so its URL must be https
	// (loopback http is allowed for local dev against a dev control plane).
	// A purely local, unexposed node never talks to the control plane, so
	// its URL is not security-sensitive.
	if c.Mode == ModeGateway || c.Exposed() {
		if err := requireSecureURL("control_plane.url", c.ControlPlane.URL); err != nil {
			return err
		}
	}
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	if err := validPort("lan.port", c.LAN.Port); err != nil {
		return err
	}
	if err := validPort("admin.port", c.Admin.Port); err != nil {
		return err
	}
	return nil
}

// requireSecureURL accepts https:// URLs, plus http:// only for loopback
// hosts (localhost / 127.0.0.1 / ::1) so local dev still works. Anything
// else — plaintext http to a remote host — would expose credentials on the
// wire and is rejected.
func requireSecureURL(name, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s must not be empty when the node relays or is exposed", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", name, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return fmt.Errorf("%s must use https:// for non-loopback hosts (got plaintext %q); "+
			"http:// is only allowed for localhost dev", name, raw)
	default:
		return fmt.Errorf("%s must be an http(s) URL, got scheme %q", name, u.Scheme)
	}
}

func validPort(name string, p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("%s must be in 1..65535, got %d", name, p)
	}
	return nil
}

// ResolvedDataDir returns DataDir with a leading "~" expanded to the user's
// home directory.
func (c *Config) ResolvedDataDir() (string, error) {
	return ExpandTilde(c.DataDir)
}

// ExpandTilde expands a leading "~" or "~/" path prefix.
func ExpandTilde(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding %q: %w", path, err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}
