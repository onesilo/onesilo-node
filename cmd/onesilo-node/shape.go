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
	// ShapeAgents serves software on this machine. Loopback only.
	ShapeAgents Shape = "agents"
	// ShapeNetwork adds LAN discovery for people's devices here.
	ShapeNetwork Shape = "network"
	// ShapeAnywhere adds a tunnel and control-plane registration.
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
			"at home or in the office.",
		},
		tradeoff: "it advertises itself on your local network and accepts connections from it.",
	},
	ShapeAnywhere: {
		label:   "Serve people anywhere",
		goodFor: "humans using this node anywhere",
		detail: []string{
			"The same devices, off your network, over an encrypted tunnel",
			"registered with One Silo.",
		},
		tradeoff: "needs sign-in and a subscription; running locally is always free.",
		followUp: "Remote access is not on yet — turn it on from the panel to sign in and open the tunnel.",
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
// Only the reach settings are touched. Capabilities are what the node can
// do, which is a different question from who can reach it, and an operator
// who turned one off should not have it turned back on by answering this.
func applyShape(cfg *config.Config, shape Shape) {
	switch shape {
	case ShapeAgents:
		cfg.Mode = config.ModeLocal
		cfg.LAN.Enabled = false
	case ShapeNetwork:
		cfg.Mode = config.ModeLocal
		cfg.LAN.Enabled = true
	case ShapeAnywhere:
		// Remote access is arranged interactively (sign-in, subscription
		// check, tunnel choice) by the panel's own flow — this records the
		// intent and leaves that alone rather than half-configuring it.
		cfg.Mode = config.ModeLocal
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
func announceShape(p *prompter, shape Shape) {
	info := shapeInfos[shape]
	p.printf("Serving: %s — good for %s.\n", info.label, info.goodFor)
	if info.followUp != "" {
		p.printf("  %s\n", info.followUp)
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
