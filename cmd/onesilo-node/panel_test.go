package main

import (
	"strings"
	"testing"

	"github.com/onesilo/onesilo-node/internal/adminapi"
	"github.com/onesilo/onesilo-node/internal/config"
)

func panelTestConfig() config.Config {
	cfg := config.Default()
	applyProductDefaults(&cfg)
	return cfg
}

func TestApplyProductDefaults(t *testing.T) {
	cfg := config.Default()
	applyProductDefaults(&cfg)
	if cfg.Mode != config.ModeLocal {
		t.Errorf("mode = %q, want %q", cfg.Mode, config.ModeLocal)
	}
	if !cfg.Capabilities.Compute || !cfg.Capabilities.Memory || !cfg.LAN.Enabled {
		t.Errorf("product defaults must enable compute, memory, and LAN; got %+v / lan=%v",
			cfg.Capabilities, cfg.LAN.Enabled)
	}
	if cfg.Tunnel.Mode != config.TunnelModeOff {
		t.Errorf("product defaults must not expose the node; tunnel mode = %q", cfg.Tunnel.Mode)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("product defaults must validate: %v", err)
	}
}

func TestCapabilitiesLine(t *testing.T) {
	cfg := panelTestConfig()
	if got := capabilitiesLine(cfg); got != "Compute, Memory, LAN" {
		t.Errorf("defaults line = %q, want %q", got, "Compute, Memory, LAN")
	}
	cfg.Capabilities.Compute = false
	if got := capabilitiesLine(cfg); got != "Memory, LAN" {
		t.Errorf("line = %q, want %q", got, "Memory, LAN")
	}
	cfg.Capabilities.Memory = false
	cfg.LAN.Enabled = false
	if got := capabilitiesLine(cfg); got != "none" {
		t.Errorf("empty line = %q, want %q", got, "none")
	}
}

func TestMenuLabelsToggleWithState(t *testing.T) {
	cfg := panelTestConfig()

	if got := modeName(cfg); got != "Local" {
		t.Errorf("modeName = %q, want Local", got)
	}
	if got := modeMenuLabel(cfg); got != "Switch to Local Relay" {
		t.Errorf("modeMenuLabel = %q", got)
	}
	cfg.Mode = config.ModeGateway
	if got := modeName(cfg); got != "Local Relay" {
		t.Errorf("modeName = %q, want Local Relay", got)
	}
	if got := modeMenuLabel(cfg); got != "Switch to Local Node" {
		t.Errorf("modeMenuLabel = %q", got)
	}

	if got := remoteMenuLabel(cfg); got != "Enable access from anywhere" {
		t.Errorf("remoteMenuLabel = %q", got)
	}
	cfg.Tunnel.Mode = config.TunnelModeManaged
	if got := remoteMenuLabel(cfg); got != "Disable access from anywhere" {
		t.Errorf("remoteMenuLabel = %q", got)
	}
}

func TestRemoteLine(t *testing.T) {
	cfg := panelTestConfig()
	var st adminapi.Status

	if got := remoteLine(cfg, st); !strings.Contains(got, "off") {
		t.Errorf("unexposed remoteLine = %q, want an off state", got)
	}

	cfg.Tunnel.Mode = config.TunnelModeManaged
	if got := remoteLine(cfg, st); !strings.Contains(got, "waiting") {
		t.Errorf("provisioning remoteLine = %q, want a waiting state", got)
	}

	st.Tunnel.URL = "https://node-abc.onesilo.dev"
	if got := remoteLine(cfg, st); got != "https://node-abc.onesilo.dev" {
		t.Errorf("provisioned remoteLine = %q, want the URL", got)
	}

	cfg.Tunnel.Mode = config.TunnelModeExternal
	cfg.Tunnel.ExternalURL = "https://node.example.com"
	st.Tunnel.URL = ""
	if got := remoteLine(cfg, st); !strings.Contains(got, "https://node.example.com") {
		t.Errorf("external remoteLine = %q, want the external URL", got)
	}
}

func TestRenderPanelScreen(t *testing.T) {
	cfg := panelTestConfig()
	st := adminapi.Status{
		Capabilities: []adminapi.CapabilityStatus{
			{Name: "compute", Enabled: true, Healthy: true},
			{Name: "memory", Enabled: true, Healthy: false, Detail: "embed model missing"},
		},
	}

	out := renderPanelScreen(cfg, st, 1, "/tmp/node.log", "hello")

	for _, want := range []string{
		"Welcome to One Silo Node",
		"Current Configuration:",
		"Mode:          Local",
		"Capabilities:  Compute, Memory, LAN",
		"1. Launch Admin Interface (127.0.0.1:8766)",
		"2. Switch to Local Relay",
		"3. Enable access from anywhere",
		"q. Quit",
		"memory: embed model missing",
		"1 device(s) waiting for pairing confirmation",
		"/tmp/node.log",
		"hello",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("panel screen missing %q:\n%s", want, out)
		}
	}

	// The healthy capability must not produce a warning line.
	if strings.Contains(out, "! compute") {
		t.Errorf("healthy capability rendered a warning:\n%s", out)
	}

	// No notice, no pairings — the optional lines disappear.
	quietOut := renderPanelScreen(cfg, adminapi.Status{}, 0, "/tmp/node.log", "")
	if strings.Contains(quietOut, "pairing confirmation") || strings.Contains(quietOut, "hello") {
		t.Errorf("optional lines rendered when empty:\n%s", quietOut)
	}
}

func TestHandleMenuDispatch(t *testing.T) {
	// handle's quit/unknown paths don't touch the node, so a zero panel
	// with only a prompter is safe to drive.
	pl := &panel{p: &prompter{out: &strings.Builder{}}}

	for _, q := range []string{"q", "Q", "quit", "exit"} {
		if _, quit := pl.handle(t.Context(), q); !quit {
			t.Errorf("handle(%q) must quit", q)
		}
	}
	if notice, quit := pl.handle(t.Context(), ""); quit || notice != "" {
		t.Errorf("empty input must refresh silently, got %q quit=%v", notice, quit)
	}
	if notice, quit := pl.handle(t.Context(), "banana"); quit || !strings.Contains(notice, "banana") {
		t.Errorf("unknown input must explain itself, got %q quit=%v", notice, quit)
	}
}
