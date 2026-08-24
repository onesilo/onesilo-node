package node

import (
	"log/slog"
	"testing"

	"github.com/onesilo/onesilo-node/internal/config"
	"github.com/onesilo/onesilo-node/internal/openaiapi"
)

func newProvider(t *testing.T, cfg config.Config) (*inferenceKeyProvider, *openaiapi.KeyStore) {
	t.Helper()
	keys, err := openaiapi.LoadKeyStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadKeyStore: %v", err)
	}
	return &inferenceKeyProvider{
		getCfg: func() config.Config { return cfg },
		keys:   keys,
		logger: slog.New(slog.DiscardHandler),
	}, keys
}

func publishingCfg() config.Config {
	cfg := config.Default()
	cfg.Capabilities.Compute = true
	cfg.OpenAI.Enabled = true
	cfg.OpenAI.PublishKeyToControlPlane = true
	return cfg
}

func TestNoKeyIsMintedUnlessPublishingIsTurnedOn(t *testing.T) {
	// The default. A node that never opted in must not hand the control
	// plane a way onto its hardware, and must not quietly mint keys either.
	cfg := publishingCfg()
	cfg.OpenAI.PublishKeyToControlPlane = false

	p, keys := newProvider(t, cfg)
	got, commit, err := p.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got != "" {
		t.Errorf("minted %q with publishing off", got)
	}
	if commit != nil {
		t.Error("a commit was returned with nothing to commit")
	}
	if keys.Count() != 0 {
		t.Errorf("key store holds %d key(s) with publishing off", keys.Count())
	}
}

func TestPublishingRequiresTheSurfaceToBeServing(t *testing.T) {
	// A key to a surface that 404s is worse than no key: the control plane
	// would dial a node that can never answer, and wait out the timeout to
	// find out. Checked live, because either switch can be flipped while
	// the node runs and the next re-registration must stop publishing.
	for name, mutate := range map[string]func(*config.Config){
		"openai surface off": func(c *config.Config) { c.OpenAI.Enabled = false },
		"compute off":        func(c *config.Config) { c.Capabilities.Compute = false },
	} {
		cfg := publishingCfg()
		mutate(&cfg)
		p, keys := newProvider(t, cfg)
		got, _, err := p.Mint()
		if err != nil {
			t.Fatalf("%s: Mint: %v", name, err)
		}
		if got != "" {
			t.Errorf("%s: minted %q anyway", name, got)
		}
		if keys.Count() != 0 {
			t.Errorf("%s: minted a key into the store anyway", name)
		}
	}
}

func TestPublishingMintsAKeyThatAuthenticates(t *testing.T) {
	p, keys := newProvider(t, publishingCfg())
	got, _, err := p.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got == "" {
		t.Fatal("nothing minted with publishing on")
	}
	if !keys.Verify(got) {
		t.Error("the published key does not authenticate against the surface")
	}
}

func TestTheOldKeyKeepsWorkingUntilTheNewOneIsCommitted(t *testing.T) {
	p, keys := newProvider(t, publishingCfg())
	first, firstCommit, _ := p.Mint()
	firstCommit()

	second, secondCommit, err := p.Mint()
	if err != nil {
		t.Fatalf("second Mint: %v", err)
	}
	if !keys.Verify(first) {
		t.Error("the previous key stopped working before the new one was committed")
	}

	secondCommit()
	if keys.Verify(first) {
		t.Error("the previous key still works after the new one was committed")
	}
	if !keys.Verify(second) {
		t.Error("committing revoked the key it was supposed to keep")
	}
}

func TestRotationDoesNotAccumulateKeys(t *testing.T) {
	// Registration runs on every tunnel restart. Without the revoke, a
	// long-lived node would collect a key per restart, each one still valid.
	p, keys := newProvider(t, publishingCfg())
	for i := 0; i < 5; i++ {
		_, commit, err := p.Mint()
		if err != nil {
			t.Fatalf("Mint %d: %v", i, err)
		}
		commit()
	}
	if keys.Count() != 1 {
		t.Errorf("key store holds %d keys after 5 rotations, want 1", keys.Count())
	}
}

func TestTurningPublishingOffDoesNotRevokeTheKeyAlreadyGranted(t *testing.T) {
	// Off means "stop renewing the grant", not "revoke it". Revoking is a
	// deliberate act through the admin API, not a side effect of a config
	// edit -- and the backend keeps the key it holds when the field is
	// absent, so a silent revoke here would break indexing with nothing
	// saying why.
	cfg := publishingCfg()
	keys, _ := openaiapi.LoadKeyStore(t.TempDir())
	p := &inferenceKeyProvider{
		getCfg: func() config.Config { return cfg },
		keys:   keys,
		logger: slog.New(slog.DiscardHandler),
	}
	granted, commit, _ := p.Mint()
	commit()

	cfg.OpenAI.PublishKeyToControlPlane = false
	got, _, err := p.Mint()
	if err != nil {
		t.Fatalf("Mint after switching off: %v", err)
	}
	if got != "" {
		t.Errorf("still publishing after the switch went off: %q", got)
	}
	if !keys.Verify(granted) {
		t.Error("switching publishing off revoked the key already granted")
	}
}
