package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/BurntSushi/toml"
)

// Setting describes one overridable configuration knob: its CLI flag name,
// its SILO_NODE_* environment variable, and how a raw string value is
// applied to a Config. A single table drives both flag registration and the
// load precedence, so the two can never drift.
type Setting struct {
	Flag  string
	Env   string
	Usage string
	Set   func(c *Config, raw string) error
}

// Settings returns the full override table.
func Settings() []Setting {
	return []Setting{
		{"mode", "SILO_NODE_MODE", "node mode: local (self-contained) | gateway (control-plane relay)",
			func(c *Config, v string) error { c.Mode = v; return nil }},
		{"data-dir", "SILO_NODE_DATA_DIR", "directory for node state (device id, pairing key)",
			func(c *Config, v string) error { c.DataDir = v; return nil }},
		{"log-format", "SILO_NODE_LOG_FORMAT", "log output format: text|json",
			func(c *Config, v string) error { c.Log.Format = v; return nil }},
		{"log-level", "SILO_NODE_LOG_LEVEL", "log level: debug|info|warn|error",
			func(c *Config, v string) error { c.Log.Level = v; return nil }},
		{"memory", "SILO_NODE_MEMORY", "enable the memory capability (true|false)",
			boolSetting(func(c *Config, v bool) { c.Capabilities.Memory = v })},
		{"compute", "SILO_NODE_COMPUTE", "enable the compute capability (true|false)",
			boolSetting(func(c *Config, v bool) { c.Capabilities.Compute = v })},
		{"control-plane-url", "SILO_NODE_CONTROL_PLANE_URL", "base URL of the Silo control plane",
			func(c *Config, v string) error { c.ControlPlane.URL = v; return nil }},
		{"auth-mode", "SILO_NODE_AUTH_MODE", "control plane auth mode: jwt|api_key|oauth",
			func(c *Config, v string) error { c.ControlPlane.AuthMode = v; return nil }},
		{"device-name", "SILO_NODE_DEVICE_NAME", "device name reported to the control plane (default: hostname)",
			func(c *Config, v string) error { c.ControlPlane.DeviceName = v; return nil }},
		{"memory-embed-model", "SILO_NODE_MEMORY_EMBED_MODEL", "Ollama embedding model for hybrid memory recall",
			func(c *Config, v string) error { c.Memory.EmbedModel = v; return nil }},
		{"ollama-host", "SILO_NODE_OLLAMA_HOST", "base URL of the Ollama server",
			func(c *Config, v string) error { c.Ollama.Host = v; return nil }},
		{"ollama-manage", "SILO_NODE_OLLAMA_MANAGE", "spawn `ollama serve` when unreachable (true|false)",
			boolSetting(func(c *Config, v bool) { c.Ollama.Manage = v })},
		{"ollama-default-model", "SILO_NODE_OLLAMA_DEFAULT_MODEL", "preferred Ollama model tag",
			func(c *Config, v string) error { c.Ollama.DefaultModel = v; return nil }},
		{"ollama-binary", "SILO_NODE_OLLAMA_BINARY", "path to the ollama binary (managed mode)",
			func(c *Config, v string) error { c.Ollama.BinaryPath = v; return nil }},
		{"tunnel-mode", "SILO_NODE_TUNNEL_MODE", "tunnel mode: off|quick|external",
			func(c *Config, v string) error { c.Tunnel.Mode = v; return nil }},
		{"cloudflared-path", "SILO_NODE_CLOUDFLARED_PATH", "path to the cloudflared binary (quick mode)",
			func(c *Config, v string) error { c.Tunnel.CloudflaredPath = v; return nil }},
		{"tunnel-external-url", "SILO_NODE_TUNNEL_EXTERNAL_URL", "public https URL (external mode)",
			func(c *Config, v string) error { c.Tunnel.ExternalURL = v; return nil }},
		{"lan-enabled", "SILO_NODE_LAN_ENABLED", "enable LAN serving (stub, later phase) (true|false)",
			boolSetting(func(c *Config, v bool) { c.LAN.Enabled = v })},
		{"lan-port", "SILO_NODE_LAN_PORT", "LAN serving port (the quick tunnel targets this port)",
			intSetting(func(c *Config, v int) { c.LAN.Port = v })},
		{"admin-port", "SILO_NODE_ADMIN_PORT", "localhost admin API port",
			intSetting(func(c *Config, v int) { c.Admin.Port = v })},
	}
}

func boolSetting(apply func(*Config, bool)) func(*Config, string) error {
	return func(c *Config, raw string) error {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("expected true|false, got %q", raw)
		}
		apply(c, v)
		return nil
	}
}

func intSetting(apply func(*Config, int)) func(*Config, string) error {
	return func(c *Config, raw string) error {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("expected an integer, got %q", raw)
		}
		apply(c, v)
		return nil
	}
}

// LoadOptions parameterizes Load.
type LoadOptions struct {
	// Path is the TOML config file. A missing file is not an error (defaults
	// apply); a malformed one is.
	Path string
	// FlagValues holds only the flags explicitly set on the command line
	// (flag name -> raw value). Highest precedence.
	FlagValues map[string]string
	// LookupEnv defaults to os.LookupEnv; injectable for tests.
	LookupEnv func(string) (string, bool)
}

// DefaultPath returns the default config file location
// (~/.silo-node/config.toml).
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(home, ".silo-node", "config.toml")
}

// Load builds a Config with precedence: flags > env > TOML file > defaults.
func Load(opts LoadOptions) (Config, error) {
	cfg := Default()

	if opts.Path != "" {
		if _, err := os.Stat(opts.Path); err == nil {
			meta, err := toml.DecodeFile(opts.Path, &cfg)
			if err != nil {
				return cfg, fmt.Errorf("parsing config file %s: %w", opts.Path, err)
			}
			if undecoded := meta.Undecoded(); len(undecoded) > 0 {
				return cfg, fmt.Errorf("unknown key %q in config file %s", undecoded[0].String(), opts.Path)
			}
		} else if !os.IsNotExist(err) {
			return cfg, fmt.Errorf("reading config file %s: %w", opts.Path, err)
		}
	}

	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	for _, s := range Settings() {
		if raw, ok := lookup(s.Env); ok {
			if err := s.Set(&cfg, raw); err != nil {
				return cfg, fmt.Errorf("%s: %w", s.Env, err)
			}
		}
	}

	for _, s := range Settings() {
		if raw, ok := opts.FlagValues[s.Flag]; ok {
			if err := s.Set(&cfg, raw); err != nil {
				return cfg, fmt.Errorf("-%s: %w", s.Flag, err)
			}
		}
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Save writes cfg as TOML to path (0600), creating parent directories.
// Used by the admin API's PUT /v1/config to persist live changes.
func Save(cfg Config, path string) error {
	// 0700: config lives in data_dir alongside the node's secret files.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	enc := toml.NewEncoder(f)
	if err := enc.Encode(cfg); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}
