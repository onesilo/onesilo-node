// Package config defines the typed silo-node configuration and its
// load/save machinery. Precedence: flags > SILO_NODE_* env > TOML file >
// defaults (see load.go).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Node modes.
const (
	// ModeLocal is a self-contained node: local memory and local LLM only.
	// The node never talks to the One Silo control plane.
	ModeLocal = "local"
	// ModeGateway is a control-plane relay: the node connects to One Silo
	// with its own credentials and exposes the cloud surface (cloud silos,
	// connectors, MCP) to local clients, alongside any local capabilities.
	// Tunnels and destination registration are gateway-mode features.
	ModeGateway = "gateway"
)

// Tunnel modes.
const (
	TunnelModeOff      = "off"
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
			Enabled: false,
			Port:    8765,
		},
		Admin: Admin{
			Port: 8766,
		},
	}
}

// Validate checks enum fields, port ranges, and mode consistency.
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeLocal, ModeGateway:
	default:
		return fmt.Errorf("mode must be %q or %q, got %q", ModeLocal, ModeGateway, c.Mode)
	}
	if c.Mode == ModeLocal && c.Tunnel.Mode != TunnelModeOff {
		return fmt.Errorf("tunnel.mode %q requires mode %q — a %q node never talks to the control plane",
			c.Tunnel.Mode, ModeGateway, ModeLocal)
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
	switch c.Tunnel.Mode {
	case TunnelModeOff, TunnelModeQuick, TunnelModeExternal:
	default:
		return fmt.Errorf("tunnel.mode must be one of off|quick|external, got %q", c.Tunnel.Mode)
	}
	if c.Tunnel.Mode == TunnelModeExternal {
		if !strings.HasPrefix(c.Tunnel.ExternalURL, "https://") {
			return fmt.Errorf("tunnel.external_url must be an https:// URL when tunnel.mode is %q", TunnelModeExternal)
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
