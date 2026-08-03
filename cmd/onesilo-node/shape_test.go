package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/onesilo/onesilo-node/internal/config"
)

func TestParseShape(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Shape
		ok   bool
	}{
		{"agents", ShapeAgents, true},
		{"network", ShapeNetwork, true},
		{"anywhere", ShapeAnywhere, true},
		{"  Network  ", ShapeNetwork, true},
		{"ANYWHERE", ShapeAnywhere, true},
		{"", "", false},
		{"lan", "", false},
		{"everyone", "", false},
	} {
		got, ok := parseShape(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseShape(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestApplyShapeSetsReach(t *testing.T) {
	for _, tc := range []struct {
		shape   Shape
		wantLAN bool
	}{
		{ShapeAgents, false},
		{ShapeNetwork, true},
		{ShapeAnywhere, true},
	} {
		cfg := config.Default()
		cfg.LAN.Enabled = !tc.wantLAN
		applyShape(&cfg, tc.shape)
		if cfg.LAN.Enabled != tc.wantLAN {
			t.Errorf("%s: lan = %v, want %v", tc.shape, cfg.LAN.Enabled, tc.wantLAN)
		}
		if cfg.Mode != config.ModeLocal {
			t.Errorf("%s: mode = %q, want %q", tc.shape, cfg.Mode, config.ModeLocal)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: config must validate: %v", tc.shape, err)
		}
	}
}

// A shape answers who can reach the node, not what it can do. An operator who
// turned compute off should not get it back by saying "serve my phone".
func TestApplyShapeLeavesCapabilitiesAlone(t *testing.T) {
	cfg := config.Default()
	cfg.Capabilities.Compute = false
	cfg.Capabilities.Memory = false
	applyShape(&cfg, ShapeAnywhere)
	if cfg.Capabilities.Compute || cfg.Capabilities.Memory {
		t.Errorf("applyShape must not touch capabilities; got %+v", cfg.Capabilities)
	}
}

func TestDescribeShape(t *testing.T) {
	cfg := config.Default()

	cfg.LAN.Enabled = false
	if got := describeShape(cfg, false); got != ShapeAgents {
		t.Errorf("loopback-only = %q, want %q", got, ShapeAgents)
	}

	cfg.LAN.Enabled = true
	if got := describeShape(cfg, false); got != ShapeNetwork {
		t.Errorf("lan = %q, want %q", got, ShapeNetwork)
	}

	// Remote reach subsumes the rest: once it is reachable off-network,
	// saying "on this network" would understate it.
	if got := describeShape(cfg, true); got != ShapeAnywhere {
		t.Errorf("remote = %q, want %q", got, ShapeAnywhere)
	}
	cfg.LAN.Enabled = false
	if got := describeShape(cfg, true); got != ShapeAnywhere {
		t.Errorf("remote without lan = %q, want %q", got, ShapeAnywhere)
	}
}

func shapePrompter(input string) (*prompter, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return &prompter{
		in:  bufio.NewReader(strings.NewReader(input)),
		out: out,
	}, out
}

func TestAskShapeAcceptsNumbersAndNames(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  Shape
	}{
		{"1\n", ShapeAgents},
		{"2\n", ShapeNetwork},
		{"3\n", ShapeAnywhere},
		{"network\n", ShapeNetwork},
		{"  Anywhere \n", ShapeAnywhere},
		{"\n", ShapeAgents},        // bare enter takes the default
		{"", ShapeAgents},          // EOF (piped, no input) takes the default
		{"yes\n2\n", ShapeNetwork}, // unrecognised input re-asks
	} {
		p, _ := shapePrompter(tc.input)
		if got := askShape(p, ShapeAgents); got != tc.want {
			t.Errorf("askShape(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// The trade-offs are the reason the picker exists: an operator choosing wider
// reach should see the cost in the same breath as the option.
func TestAskShapePrintsGoodForAndTradeoffs(t *testing.T) {
	p, out := shapePrompter("1\n")
	askShape(p, ShapeAgents)
	text := out.String()

	for _, want := range []string{
		"local agents using this node",
		"humans using this node on this network",
		"humans using this node anywhere",
		"advertises itself on your local network",
		// Discovery is a broadcast; access is not. Stating the first
		// without the second reads as "anyone on the wifi can use it",
		// which is both wrong and enough to scare someone off the setting
		// they actually want.
		"Approved devices only, end-to-end encrypted",
		"needs sign-in and a subscription",
		"(default)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("picker output missing %q\n---\n%s", want, text)
		}
	}
}

// Picking "anywhere" configures LAN and records the intent, but sign-in and
// the tunnel are the panel's job. Saying "serving people anywhere" and
// stopping there would claim reach the node does not have.
func TestAnnounceShapeAdmitsUnfinishedRemoteSetup(t *testing.T) {
	p, out := shapePrompter("")
	announceShape(p, ShapeAnywhere)
	if !strings.Contains(out.String(), "not on yet") {
		t.Errorf("anywhere must say remote access is not live yet; got %q", out.String())
	}

	// The shapes that are complete on exit should not imply leftover work.
	for _, shape := range []Shape{ShapeAgents, ShapeNetwork} {
		p, out := shapePrompter("")
		announceShape(p, shape)
		if strings.Contains(out.String(), "not on yet") {
			t.Errorf("%s is fully applied; got %q", shape, out.String())
		}
	}
}

// A scripted install — onesilo-buzz provisioning a node for itself, CI, a
// Dockerfile — is exactly where nobody is watching, so it must not widen reach.
func TestAskShapeNonInteractiveTakesTheDefault(t *testing.T) {
	p, out := shapePrompter("3\n")
	p.assumeYes = true
	if got := askShape(p, ShapeAgents); got != ShapeAgents {
		t.Errorf("assumeYes = %q, want %q", got, ShapeAgents)
	}
	if !strings.Contains(out.String(), "non-interactive") {
		t.Errorf("assumeYes should say it answered for you; got %q", out.String())
	}
}
