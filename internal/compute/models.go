package compute

import (
	"context"

	"github.com/onesilo/onesilo-node/internal/compute/ollama"
)

// InstalledModels lists the models installed on the Ollama server. The
// server must be reachable (it is not spawned for a listing).
func (c *Capability) InstalledModels(ctx context.Context) ([]ollama.Model, error) {
	return c.manager.Client().Tags(ctx)
}

// PullModel downloads a model, spawning the managed server first when
// needed. Progress chunks stream to onProgress (may be nil).
func (c *Capability) PullModel(ctx context.Context, model string, onProgress func(ollama.PullProgress)) error {
	if err := c.manager.EnsureRunning(ctx); err != nil {
		return err
	}
	return c.manager.Client().Pull(ctx, model, onProgress)
}
