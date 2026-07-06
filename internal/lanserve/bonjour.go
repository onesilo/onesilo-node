package lanserve

import (
	"fmt"
	"os"
	"strings"

	"github.com/libp2p/zeroconf/v2"
)

// Bonjour service constants — must match the iOS BonjourDiscoveryService
// and SiloMac's BonjourPublisher.
const (
	bonjourServiceType = "_silo-llm._tcp"
	bonjourDomain      = "local."
)

// Announcer publishes the LAN service over mDNS/Bonjour. Behind an
// interface so tests don't need real multicast networking.
type Announcer interface {
	// Announce registers the service instance. txt entries are "key=value".
	Announce(instance string, port int, txt []string) error
	// Update replaces the TXT record of a live registration.
	Update(txt []string) error
	// Shutdown unregisters the service. Safe to call when not announced.
	Shutdown()
}

// NewZeroconfAnnouncer returns the production Announcer backed by
// libp2p/zeroconf.
func NewZeroconfAnnouncer() Announcer { return &zeroconfAnnouncer{} }

type zeroconfAnnouncer struct {
	srv *zeroconf.Server
}

func (z *zeroconfAnnouncer) Announce(instance string, port int, txt []string) error {
	if z.srv != nil {
		return fmt.Errorf("already announced")
	}
	srv, err := zeroconf.Register(instance, bonjourServiceType, bonjourDomain, port, txt, nil)
	if err != nil {
		return fmt.Errorf("registering bonjour service: %w", err)
	}
	z.srv = srv
	return nil
}

func (z *zeroconfAnnouncer) Update(txt []string) error {
	if z.srv == nil {
		return fmt.Errorf("not announced")
	}
	z.srv.SetText(txt)
	return nil
}

func (z *zeroconfAnnouncer) Shutdown() {
	if z.srv != nil {
		z.srv.Shutdown()
		z.srv = nil
	}
}

// BonjourInstanceName returns "<Hostname> Silo LLM", matching SiloMac's
// "<localizedName> Silo LLM".
func BonjourInstanceName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "Silo Node"
	}
	// Strip the ".local"/".lan" suffix macOS/Linux hostnames often carry.
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return host + " Silo LLM"
}

// BonjourTXT builds the TXT record: the keys SiloMac publishes (model,
// version, capabilities — iOS parses "model") plus an additive protocol=1.
func BonjourTXT(model string) []string {
	return []string{
		"model=" + model,
		"version=1.0",
		"capabilities=chat,stream,e2e",
		"protocol=1",
	}
}
