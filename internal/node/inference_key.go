package node

import (
	"log/slog"

	"github.com/onesilo/onesilo-node/internal/config"
	"github.com/onesilo/onesilo-node/internal/openaiapi"
)

// inferenceKeyProvider mints the OpenAI-surface key the node publishes to
// the control plane at registration (SILO-764), so the control plane can
// run inference here on the owner's behalf — indexing an artifact marked
// local_only is the first use (SILO-757).
//
// It sits in internal/node rather than internal/controlplane because it is
// where a capability and the config meet, and controlplane must not need to
// know either. The registration manager sees only the interface.
type inferenceKeyProvider struct {
	getCfg func() config.Config
	keys   *openaiapi.KeyStore
	logger *slog.Logger
}

// Mint implements controlplane.InferenceKeyProvider.
//
// Returns an empty key — publishing nothing — unless the operator turned on
// openai.publish_key_to_control_plane *and* the surface is actually serving.
// Both are checked live rather than at construction, because either can be
// switched off while the node runs and a re-registration must stop
// publishing when they are: handing out a key to a surface that now 404s
// would leave the control plane dialling a node that can never answer.
//
// Publishing nothing is not a revocation. The control plane keeps the key
// it already holds when the field is absent, which is what makes a
// tunnel-only re-register cheap. Turning the switch off stops *renewing*
// the grant; revoking it is the admin API's job, because that is a
// deliberate act the operator should see rather than a side effect of a
// config edit.
func (p *inferenceKeyProvider) Mint() (string, func(), error) {
	cfg := p.getCfg()
	if !cfg.OpenAI.PublishKeyToControlPlane || !cfg.OpenAI.Enabled || !cfg.Capabilities.Compute {
		return "", nil, nil
	}

	plaintext, superseded, err := p.keys.MintForControlPlane()
	if err != nil {
		return "", nil, err
	}

	commit := func() {
		if len(superseded) == 0 {
			return
		}
		// The control plane has the new key now, so the ones it replaces
		// can go. A failure here leaks a stale key into the store rather
		// than breaking anything, so it is logged, not returned.
		if err := p.keys.RevokeAll(superseded); err != nil {
			p.logger.Warn("could not revoke superseded control-plane key(s)",
				"count", len(superseded), "error", err)
		}
	}
	return plaintext, commit, nil
}
