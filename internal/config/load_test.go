package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) (string, bool) { return "", false }

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(LoadOptions{LookupEnv: noEnv})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != "~/.silo-node" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
	if cfg.ControlPlane.URL != "https://api.onesilo.com" {
		t.Errorf("ControlPlane.URL = %q", cfg.ControlPlane.URL)
	}
	if cfg.ControlPlane.AuthMode != AuthModeJWT {
		t.Errorf("AuthMode = %q", cfg.ControlPlane.AuthMode)
	}
	if cfg.Ollama.Host != "http://127.0.0.1:11434" || cfg.Ollama.DefaultModel != "llama3.2:3b" {
		t.Errorf("Ollama defaults = %+v", cfg.Ollama)
	}
	if cfg.Tunnel.Mode != TunnelModeOff {
		t.Errorf("Tunnel.Mode = %q", cfg.Tunnel.Mode)
	}
	if cfg.LAN.Port != 8765 || cfg.Admin.Port != 8766 {
		t.Errorf("ports = %d/%d", cfg.LAN.Port, cfg.Admin.Port)
	}
	if cfg.Capabilities.Compute || cfg.Capabilities.Memory {
		t.Errorf("capabilities should default off: %+v", cfg.Capabilities)
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFileOverridesDefaults(t *testing.T) {
	path := writeFile(t, `
data_dir = "/var/lib/silo-node"

[capabilities]
compute = true

[ollama]
default_model = "qwen2.5:7b"

[admin]
port = 9000
`)
	cfg, err := Load(LoadOptions{Path: path, LookupEnv: noEnv})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != "/var/lib/silo-node" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
	if !cfg.Capabilities.Compute {
		t.Error("compute should be enabled from file")
	}
	if cfg.Ollama.DefaultModel != "qwen2.5:7b" {
		t.Errorf("DefaultModel = %q", cfg.Ollama.DefaultModel)
	}
	if cfg.Admin.Port != 9000 {
		t.Errorf("Admin.Port = %d", cfg.Admin.Port)
	}
	// Untouched key keeps its default.
	if cfg.Ollama.Host != "http://127.0.0.1:11434" {
		t.Errorf("Ollama.Host = %q", cfg.Ollama.Host)
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	path := writeFile(t, `
[ollama]
default_model = "from-file:1b"

[admin]
port = 9000
`)
	env := map[string]string{
		"SILO_NODE_OLLAMA_DEFAULT_MODEL": "from-env:1b",
		"SILO_NODE_COMPUTE":              "true",
	}
	cfg, err := Load(LoadOptions{
		Path:      path,
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Ollama.DefaultModel != "from-env:1b" {
		t.Errorf("env should beat file: %q", cfg.Ollama.DefaultModel)
	}
	if !cfg.Capabilities.Compute {
		t.Error("SILO_NODE_COMPUTE=true should enable compute")
	}
	if cfg.Admin.Port != 9000 {
		t.Errorf("file value without env override should hold: %d", cfg.Admin.Port)
	}
}

func TestLoadFlagsOverrideEnv(t *testing.T) {
	env := map[string]string{
		"SILO_NODE_OLLAMA_DEFAULT_MODEL": "from-env:1b",
		"SILO_NODE_ADMIN_PORT":           "9001",
	}
	cfg, err := Load(LoadOptions{
		FlagValues: map[string]string{
			"ollama-default-model": "from-flag:1b",
		},
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Ollama.DefaultModel != "from-flag:1b" {
		t.Errorf("flag should beat env: %q", cfg.Ollama.DefaultModel)
	}
	if cfg.Admin.Port != 9001 {
		t.Errorf("env value without flag override should hold: %d", cfg.Admin.Port)
	}
}

func TestLoadMissingFileIsFine(t *testing.T) {
	_, err := Load(LoadOptions{
		Path:      filepath.Join(t.TempDir(), "nope.toml"),
		LookupEnv: noEnv,
	})
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	path := writeFile(t, "not_a_real_key = true\n")
	_, err := Load(LoadOptions{Path: path, LookupEnv: noEnv})
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("expected unknown-key error, got %v", err)
	}
}

func TestLoadRejectsBadEnum(t *testing.T) {
	_, err := Load(LoadOptions{
		FlagValues: map[string]string{"tunnel-mode": "sideways"},
		LookupEnv:  noEnv,
	})
	if err == nil || !strings.Contains(err.Error(), "tunnel.mode") {
		t.Fatalf("expected tunnel.mode error, got %v", err)
	}
}

func TestLoadRejectsBadBool(t *testing.T) {
	_, err := Load(LoadOptions{
		FlagValues: map[string]string{"compute": "yep"},
		LookupEnv:  noEnv,
	})
	if err == nil || !strings.Contains(err.Error(), "true|false") {
		t.Fatalf("expected bool parse error, got %v", err)
	}
}

func TestExternalModeRequiresHTTPSURL(t *testing.T) {
	_, err := Load(LoadOptions{
		FlagValues: map[string]string{"tunnel-mode": "external"},
		LookupEnv:  noEnv,
	})
	if err == nil || !strings.Contains(err.Error(), "external_url") {
		t.Fatalf("expected external_url error, got %v", err)
	}
	_, err = Load(LoadOptions{
		FlagValues: map[string]string{
			"tunnel-mode":         "external",
			"tunnel-external-url": "https://node.example.com",
		},
		LookupEnv: noEnv,
	})
	if err != nil {
		t.Fatalf("valid external config should load: %v", err)
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	got, err := ExpandTilde("~/.silo-node")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(home, ".silo-node") {
		t.Errorf("ExpandTilde = %q", got)
	}
	got, err = ExpandTilde("/absolute/path")
	if err != nil || got != "/absolute/path" {
		t.Errorf("absolute path should pass through: %q, %v", got, err)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.Capabilities.Compute = true
	cfg.Ollama.DefaultModel = "qwen2.5:7b"
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config file mode = %v, want 0600", info.Mode().Perm())
	}
	loaded, err := Load(LoadOptions{Path: path, LookupEnv: noEnv})
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if loaded != cfg {
		t.Errorf("round trip mismatch:\n got  %+v\n want %+v", loaded, cfg)
	}
}
