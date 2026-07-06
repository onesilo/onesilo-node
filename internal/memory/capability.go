package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
	"github.com/onesilo/silo-node/internal/config"
)

// DefaultEmbedModel is used when config leaves memory.embed_model empty.
const DefaultEmbedModel = "nomic-embed-text"

// Capability implements the node Capability interface for silo memory
// (structurally; no import of internal/node).
type Capability struct {
	getCfg func() config.Config
	cipher Cipher
	embed  EmbedderSource
	logger *slog.Logger

	mu    sync.Mutex
	store *Store
}

// New builds the memory capability. embed resolves the live embedder
// (nil-safe: pass a source returning ok=false when compute is off).
func New(getCfg func() config.Config, embed EmbedderSource, logger *slog.Logger) *Capability {
	if embed == nil {
		embed = func() (Embedder, string, bool) { return nil, "", false }
	}
	return &Capability{getCfg: getCfg, cipher: PlaintextCipher{}, embed: embed, logger: logger}
}

// Name implements node.Capability.
func (c *Capability) Name() string { return "memory" }

// Enabled implements node.Capability.
func (c *Capability) Enabled() bool { return c.getCfg().Capabilities.Memory }

// Start opens the store at <data_dir>/memory.db.
func (c *Capability) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store != nil {
		return nil
	}
	cfg := c.getCfg()
	dataDir, err := cfg.ResolvedDataDir()
	if err != nil {
		return err
	}
	store, err := OpenStore(filepath.Join(dataDir, "memory.db"))
	if err != nil {
		return err
	}
	c.store = store
	c.logger.Info("memory capability started", "db", filepath.Join(dataDir, "memory.db"))
	return nil
}

// Stop closes the store.
func (c *Capability) Stop(ctx context.Context) error {
	c.mu.Lock()
	store := c.store
	c.store = nil
	c.mu.Unlock()
	if store != nil {
		if err := store.Close(); err != nil {
			return err
		}
	}
	c.logger.Info("memory capability stopped")
	return nil
}

// Healthy implements node.Capability: db open + PRAGMA quick_check.
func (c *Capability) Healthy(ctx context.Context) (bool, string) {
	store := c.currentStore()
	if store == nil {
		return false, "not started"
	}
	if err := store.QuickCheck(ctx); err != nil {
		return false, fmt.Sprintf("quick_check failed: %v", err)
	}
	silos, err := store.Silos(ctx)
	if err != nil {
		return false, err.Error()
	}
	n := 0
	for _, s := range silos {
		n += s.Count
	}
	return true, fmt.Sprintf("%d memories in %d silos", n, len(silos))
}

func (c *Capability) currentStore() *Store {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.store
}

func (c *Capability) embedModel() string {
	if m := c.getCfg().Memory.EmbedModel; m != "" {
		return m
	}
	return DefaultEmbedModel
}

// embedderSource wraps the configured source, substituting the configured
// embed model when the source doesn't dictate one.
func (c *Capability) embedderSource() EmbedderSource {
	return func() (Embedder, string, bool) {
		e, model, ok := c.embed()
		if !ok {
			return nil, "", false
		}
		if model == "" {
			model = c.embedModel()
		}
		return e, model, true
	}
}

// ErrNotRunning is returned by API operations while the capability is off.
var ErrNotRunning = fmt.Errorf("memory capability is not running")

// Remember seals and stores content in a silo, indexes it for keyword
// search, and best-effort embeds it (skipped silently when compute is
// off). Returns the new memory id.
func (c *Capability) Remember(ctx context.Context, siloID, content string, metadata map[string]any) (string, error) {
	store := c.currentStore()
	if store == nil {
		return "", ErrNotRunning
	}

	sealed, err := c.cipher.Seal(siloID, []byte(content))
	if err != nil {
		return "", err
	}
	metadataJSON := ""
	if len(metadata) > 0 {
		raw, err := json.Marshal(metadata)
		if err != nil {
			return "", fmt.Errorf("encoding metadata: %w", err)
		}
		metadataJSON = string(raw)
	}

	id := uuid.NewString()
	// Index-time plaintext is passed explicitly — see Cipher docs.
	if err := store.Insert(ctx, id, siloID, sealed, content, metadataJSON); err != nil {
		return "", err
	}

	if e, model, ok := c.embedderSource()(); ok {
		if vecs, err := e.Embed(ctx, model, []string{content}); err != nil {
			c.logger.Debug("embedding skipped", "error", err)
		} else if len(vecs) == 1 {
			if err := store.PutEmbedding(ctx, id, model, toFloat32(vecs[0])); err != nil {
				c.logger.Warn("storing embedding failed", "error", err)
			}
		}
	}
	return id, nil
}

// Recall runs hybrid (or FTS5-only) retrieval over a silo.
func (c *Capability) Recall(ctx context.Context, siloID, query string, limit int) ([]RecallResult, error) {
	store := c.currentStore()
	if store == nil {
		return nil, ErrNotRunning
	}
	return recall(ctx, store, c.cipher, c.embedderSource(), siloID, query, limit)
}

// Forget deletes one memory; false when it doesn't exist.
func (c *Capability) Forget(ctx context.Context, siloID, memoryID string) (bool, error) {
	store := c.currentStore()
	if store == nil {
		return false, ErrNotRunning
	}
	return store.Delete(ctx, siloID, memoryID)
}

// Silos lists silos with memory counts.
func (c *Capability) Silos(ctx context.Context) ([]SiloCount, error) {
	store := c.currentStore()
	if store == nil {
		return nil, ErrNotRunning
	}
	return store.Silos(ctx)
}
