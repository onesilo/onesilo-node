package node

import "context"

// Capability is one independently enable-able contribution this node makes
// to the Silo control plane (compute today; memory and LAN serving arrive
// in later phases). The lifecycle reconciler starts/stops capabilities as
// config changes, and the control-plane heartbeat maps Healthy() into
// capabilities_status.
type Capability interface {
	// Name is the stable capability identifier ("compute", "memory").
	Name() string
	// Enabled reflects the live configuration.
	Enabled() bool
	// Healthy probes liveness and returns a human-readable detail string.
	Healthy(ctx context.Context) (bool, string)
	// Start brings the capability up. Idempotent-safe under the reconciler
	// (only called when not running).
	Start(ctx context.Context) error
	// Stop tears the capability down and releases owned processes.
	Stop(ctx context.Context) error
}
