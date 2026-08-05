package main

import (
	"fmt"
	"strings"

	"github.com/onesilo/onesilo-node/internal/config"
)

// Shape is what a node is for — who it serves.
//
// Every shape is a preset over settings that already exist. The point is
// not new capability; it is that "who reaches this node" was previously
// spread across mode, capabilities, lan.enabled and the tunnel, decided by
// defaults, and discoverable only by toggling menu items. Reach is the
// single most consequential thing about a node, so it gets asked.
type Shape string

const (
	// ShapeAgents serves software on this machine. Loopback only: LAN
	// discovery off and no tunnel.
	ShapeAgents Shape = "agents"
	// ShapeNetwork adds LAN discovery for people's devices here, still
	// without a tunnel.
	ShapeNetwork Shape = "network"
	// ShapeAnywhere is LAN discovery plus remote access. Setup cannot finish
	// it — the tunnel needs sign-in and a subscription check, which is the
	// panel's interactive flow — so picking it here configures the LAN half
	// and points at the rest. It is the only shape that never narrows.
	ShapeAnywhere Shape = "anywhere"
)

// Shapes in the order they are offered: narrowest reach first, so the
// default is the one that exposes least.
var shapeOrder = []Shape{ShapeAgents, ShapeNetwork, ShapeAnywhere}

type shapeInfo struct {
	label    string
	goodFor  string
	detail   []string
	tradeoff string
	// followUp names work the picker cannot finish on its own, so choosing
	// a shape never reports reach the node does not actually have yet.
	followUp string
}

var shapeInfos = map[Shape]shapeInfo{
	ShapeAgents: {
		label:   "Serve agents on this machine",
		goodFor: "local agents using this node",
		detail: []string{
			"onesilo-buzz, an editor plugin, anything running here.",
			"Nothing listens beyond loopback and nothing is advertised.",
		},
	},
	ShapeNetwork: {
		label:   "Serve people on this network",
		goodFor: "humans using this node on this network",
		detail: []string{
			"The Silo app on your phone or laptop finds it automatically,",
			"at home or in the office. Approved devices only, end-to-end encrypted.",
		},
		// What actually leaks is the node's existence, not access to it:
		// discovery is a broadcast, but a device that has not been approved
		// here gets nothing past the handshake. Saying it "accepts
		// connections from the network" overstated that and would push
		// people away from the setting they want.
		tradeoff: "it advertises itself on your local network, so anyone there can see it exists.",
	},
	ShapeAnywhere: {
		label:   "Serve people anywhere",
		goodFor: "humans using this node anywhere",
		detail: []string{
			"The same devices, off your network, over an encrypted tunnel",
			"registered with One Silo.",
		},
		tradeoff: "needs sign-in and a subscription; running locally is always free.",
		followUp: "LAN is set up; remote access is not on yet — turn it on from the panel to sign in and open the tunnel.",
	},
}

// parseShape maps a -serve value onto a Shape.
func parseShape(s string) (Shape, bool) {
	switch Shape(strings.ToLower(strings.TrimSpace(s))) {
	case ShapeAgents:
		return ShapeAgents, true
	case ShapeNetwork:
		return ShapeNetwork, true
	case ShapeAnywhere:
		return ShapeAnywhere, true
	}
	return "", false
}

// applyShape writes a shape onto a config.
//
// Reach has two axes — LAN discovery and the tunnel — and a shape has to own
// both of them, or it is not describing reach at all. Setting LAN alone meant
// `-serve=agents` on a config with a live tunnel printed "nothing listens
// beyond loopback" at a node that was still reachable from the internet:
// exactly the gap between claim and truth this picker exists to close.
//
// So the narrowing shapes narrow both. That can switch off remote access
// someone had configured, which is the point — "serve agents on this
// machine" is an explicit request to stop serving everyone else, and it is
// one panel toggle to undo. ShapeAnywhere is the exception: it is the widest
// shape, so it only ever opens.
//
// Capabilities are left alone throughout. What a node can do is a different
// question from who can reach it, and an operator who turned one off should
// not have it turned back on by answering this.
func applyShape(cfg *config.Config, shape Shape) {
	cfg.Mode = config.ModeLocal
	switch shape {
	case ShapeAgents:
		cfg.LAN.Enabled = false
		cfg.Tunnel.Mode = config.TunnelModeOff
	case ShapeNetwork:
		cfg.LAN.Enabled = true
		cfg.Tunnel.Mode = config.TunnelModeOff
	case ShapeAnywhere:
		// The tunnel needs sign-in, a subscription check and a mode choice —
		// the panel's interactive flow, not something setup can complete.
		// Whatever it is now is left as it is: already on stays on, and off
		// stays off with announceShape saying so.
		cfg.LAN.Enabled = true
	}
}

// describeShape names the shape a config currently adds up to, so status
// output can report reach without the operator deriving it from toggles.
func describeShape(cfg config.Config, remote bool) Shape {
	if remote {
		return ShapeAnywhere
	}
	if cfg.LAN.Enabled {
		return ShapeNetwork
	}
	return ShapeAgents
}

// askShape presents the picker and returns the answer.
//
// Non-interactive runs take ShapeAgents: it is the narrowest reach, and a
// scripted install (onesilo-buzz setting up a node for itself, CI, a
// Dockerfile) is exactly the case where nobody is present to notice that a
// service started advertising itself on the network.
func askShape(p *prompter, def Shape) Shape {
	if p.assumeYes {
		p.printf("What should this node do? %s (non-interactive)\n\n", shapeInfos[def].label)
		return def
	}

	p.printf("What should this node do?\n\n")
	for i, shape := range shapeOrder {
		info := shapeInfos[shape]
		marker := ""
		if shape == def {
			marker = " (default)"
		}
		p.printf("  %d. %s%s\n", i+1, info.label, marker)
		p.printf("     Good for: %s.\n", info.goodFor)
		for _, line := range info.detail {
			p.printf("     %s\n", line)
		}
		if info.tradeoff != "" {
			p.printf("     Trade-off: %s\n", info.tradeoff)
		}
		p.printf("\n")
	}

	defIndex := 1
	for i, shape := range shapeOrder {
		if shape == def {
			defIndex = i + 1
		}
	}
	for {
		p.printf("Select 1-%d [%d]: ", len(shapeOrder), defIndex)
		line, err := p.line()
		if err != nil {
			p.printf("%d\n", defIndex)
			return def
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer == "" {
			return def
		}
		for i, shape := range shapeOrder {
			// Accept the number or the name: anyone who read the docs will
			// type "network" rather than counting.
			if answer == fmt.Sprint(i+1) || answer == string(shape) {
				return shape
			}
		}
		p.printf("Please answer 1-%d, or a name (%s).\n", len(shapeOrder), shapeNames())
	}
}

// announceShape confirms the choice in one line, plus anything the picker
// could not do for them.
//
// It reports what the config now says rather than what was asked for, so the
// line cannot outrun the settings behind it: picking "anywhere" on a node
// with no tunnel reads as the network shape it currently is, with the
// remaining step named.
func announceShape(p *prompter, cfg config.Config, shape Shape) {
	actual := describeShape(cfg, cfg.Exposed())
	info := shapeInfos[actual]
	p.printf("Serving: %s — good for %s.\n", info.label, info.goodFor)
	// Only when they asked for more than the config got. Someone who picked
	// "anywhere" on a node whose tunnel is already up is not waiting on
	// anything.
	if shape != actual {
		if follow := shapeInfos[shape].followUp; follow != "" {
			p.printf("  %s\n", follow)
		}
	}
	p.printf("\n")
}

func shapeNames() string {
	names := make([]string, 0, len(shapeOrder))
	for _, s := range shapeOrder {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}
