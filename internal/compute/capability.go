// Package compute implements the compute capability: local LLM inference
// backed by an Ollama server. It satisfies the node.Capability interface
// structurally (no import of internal/node).
package compute

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/onesilo/onesilo-node/internal/compute/ollama"
	"github.com/onesilo/onesilo-node/internal/config"
)

// ErrNoModels means the Ollama server is up but has no runnable model.
var ErrNoModels = errors.New("no models installed on the ollama server")

// Capability wires the Ollama runtime into the node lifecycle.
type Capability struct {
	getCfg  func() config.Config
	manager *ollama.Manager
	logger  *slog.Logger

	mu           sync.Mutex
	started      bool
	currentModel string
}

// New builds the compute capability. getCfg must return the live node
// config so admin-API changes take effect without a rebuild.
func New(getCfg func() config.Config, logger *slog.Logger) *Capability {
	c := &Capability{getCfg: getCfg, logger: logger}
	c.manager = ollama.NewManager(func() ollama.Settings {
		o := getCfg().Ollama
		return ollama.Settings{
			Host:         o.Host,
			Manage:       o.Manage,
			BinaryPath:   o.BinaryPath,
			DefaultModel: o.DefaultModel,
		}
	}, logger)
	return c
}

// Name implements node.Capability.
func (c *Capability) Name() string { return "compute" }

// Enabled implements node.Capability.
func (c *Capability) Enabled() bool { return c.getCfg().Capabilities.Compute }

// Start ensures the Ollama server is reachable (spawning it when managed)
// and resolves the model this node will advertise.
func (c *Capability) Start(ctx context.Context) error {
	if err := c.manager.EnsureRunning(ctx); err != nil {
		return err
	}
	model, err := ResolveRunnableModel(ctx, c.manager.Client(), c.getCfg().Ollama.DefaultModel)
	if err != nil {
		// Server is up; a missing model is a health problem, not a start
		// failure — the desktop app may pull a model right after.
		c.logger.Warn("compute started but no runnable model yet", "error", err)
	}
	c.mu.Lock()
	c.started = true
	c.currentModel = model
	c.mu.Unlock()
	c.logger.Info("compute capability started", "model", model)
	return nil
}

// Stop terminates a managed Ollama server (servers we merely connected to
// are left running).
func (c *Capability) Stop(ctx context.Context) error {
	c.manager.Stop()
	c.mu.Lock()
	c.started = false
	c.currentModel = ""
	c.mu.Unlock()
	c.logger.Info("compute capability stopped")
	return nil
}

// Healthy reports live health: server reachable and a runnable model
// resolved. Refreshes CurrentModel as a side effect.
func (c *Capability) Healthy(ctx context.Context) (bool, string) {
	c.mu.Lock()
	started := c.started
	c.mu.Unlock()
	if !started {
		return false, "not started"
	}
	if !c.manager.Reachable(ctx) {
		return false, fmt.Sprintf("ollama server not reachable at %s", c.getCfg().Ollama.Host)
	}
	model, err := ResolveRunnableModel(ctx, c.manager.Client(), c.getCfg().Ollama.DefaultModel)
	if err != nil {
		return false, err.Error()
	}
	c.mu.Lock()
	c.currentModel = model
	c.mu.Unlock()
	return true, "serving " + model
}

// CurrentModel returns the model tag this node advertises to the control
// plane ("" until resolved).
func (c *Capability) CurrentModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentModel
}

// StreamGenerate streams a completion from the node's resolved model. Used
// by the LAN serving path; ctx cancellation aborts the stream (interrupt).
func (c *Capability) StreamGenerate(ctx context.Context, prompt string, temperature float64) (<-chan ollama.GenerateResult, error) {
	stream, _, err := c.StreamGenerateModel(ctx, prompt, temperature)
	return stream, err
}

// StreamGenerateModel is StreamGenerate but also reports the model tag the
// stream actually runs on — resolved at call time, so the answer can't be
// changed out from under the caller by a concurrent CurrentModel refresh.
func (c *Capability) StreamGenerateModel(ctx context.Context, prompt string, temperature float64) (<-chan ollama.GenerateResult, string, error) {
	c.mu.Lock()
	started := c.started
	model := c.currentModel
	c.mu.Unlock()
	if !started {
		return nil, "", errors.New("compute capability not running")
	}
	if model == "" {
		resolved, err := ResolveRunnableModel(ctx, c.manager.Client(), c.getCfg().Ollama.DefaultModel)
		if err != nil {
			return nil, "", err
		}
		c.mu.Lock()
		c.currentModel = resolved
		c.mu.Unlock()
		model = resolved
	}
	stream, err := c.manager.Client().StreamGenerate(ctx, model, prompt, temperature)
	return stream, model, err
}

// Embed computes embeddings via the Ollama server (memory capability's
// hybrid recall). Satisfies memory.Embedder structurally.
func (c *Capability) Embed(ctx context.Context, model string, texts []string) ([][]float64, error) {
	return c.manager.Client().Embed(ctx, model, texts)
}

// ResolveRunnableModel picks the tag to run: the preferred (configured
// default) model when installed, otherwise the first installed model — so a
// machine with only e.g. qwen2.5:7b works without configuration. Mirrors
// SiloMac's OllamaRuntime.resolveRunnableModel.
func ResolveRunnableModel(ctx context.Context, client *ollama.Client, preferred string) (string, error) {
	models, err := client.Tags(ctx)
	if err != nil {
		return "", fmt.Errorf("listing ollama models: %w", err)
	}
	for _, m := range models {
		if m.Name == preferred {
			return preferred, nil
		}
	}
	if len(models) > 0 {
		return models[0].Name, nil
	}
	return "", ErrNoModels
}
