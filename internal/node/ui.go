package node

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/onesilo/onesilo-node/internal/adminapi"
	"github.com/onesilo/onesilo-node/internal/compute/ollama"
	"github.com/onesilo/onesilo-node/internal/logging"
	"github.com/onesilo/onesilo-node/internal/siloexport"
)

// adminapi.UIController implementation: the surface behind the embedded
// admin dashboard (Silos / Models / Settings).

// ListSilos implements adminapi.UIController.
func (n *Node) ListSilos(ctx context.Context) ([]adminapi.SiloInfo, error) {
	counts, err := n.memoryCap.Silos(ctx)
	if err != nil {
		return nil, err
	}
	silos := make([]adminapi.SiloInfo, 0, len(counts))
	for _, c := range counts {
		silos = append(silos, adminapi.SiloInfo{SiloID: c.SiloID, Count: c.Count})
	}
	return silos, nil
}

// ListMemories implements adminapi.UIController.
func (n *Node) ListMemories(ctx context.Context, siloID string) ([]adminapi.MemoryRecord, error) {
	items, err := n.memoryCap.List(ctx, siloID)
	if err != nil {
		return nil, err
	}
	records := make([]adminapi.MemoryRecord, 0, len(items))
	for _, it := range items {
		records = append(records, adminapi.MemoryRecord{
			ID:        it.ID,
			Content:   it.Content,
			Metadata:  it.Metadata,
			CreatedAt: it.CreatedAt,
			UpdatedAt: it.UpdatedAt,
		})
	}
	return records, nil
}

// ForgetMemory implements adminapi.UIController.
func (n *Node) ForgetMemory(ctx context.Context, siloID, memoryID string) (bool, error) {
	return n.memoryCap.Forget(ctx, siloID, memoryID)
}

// ExportSilo implements adminapi.UIController: stream the silo as a .silo
// package (github.com/onesilo/silo-spec).
func (n *Node) ExportSilo(ctx context.Context, siloID string, w io.Writer) error {
	items, err := n.memoryCap.List(ctx, siloID)
	if err != nil {
		return err
	}
	return siloexport.Write(w, siloID, n.deviceName(), items, time.Now())
}

// Models implements adminapi.UIController.
func (n *Node) Models(ctx context.Context) adminapi.ModelsInfo {
	cfg := n.snapshot()
	info := adminapi.ModelsInfo{
		Active:  n.computeCap.CurrentModel(),
		Default: cfg.Ollama.DefaultModel,
		Models:  []adminapi.ModelInfo{},
	}
	models, err := n.computeCap.InstalledModels(ctx)
	if err != nil {
		info.Error = err.Error()
	}
	for _, m := range models {
		info.Models = append(info.Models, adminapi.ModelInfo{
			Name:       m.Name,
			SizeBytes:  m.Size,
			ModifiedAt: m.ModifiedAt,
			Active:     m.Name == info.Active,
			Default:    m.Name == info.Default,
		})
	}
	n.pullMu.Lock()
	if n.pullState != nil {
		st := *n.pullState
		info.Pull = &st
	}
	n.pullMu.Unlock()
	return info
}

// StartPull implements adminapi.UIController: one background pull at a
// time, progress readable via Models().
func (n *Node) StartPull(model string) error {
	if !n.snapshot().Capabilities.Compute {
		return fmt.Errorf("compute capability is disabled")
	}
	n.pullMu.Lock()
	if n.pullState != nil && n.pullState.Status == "pulling" {
		inFlight := n.pullState.Model
		n.pullMu.Unlock()
		return fmt.Errorf("%w (pulling %s)", adminapi.ErrPullInProgress, inFlight)
	}
	n.pullState = &adminapi.PullState{Model: model, Status: "pulling"}
	n.pullMu.Unlock()

	// runCtx is set before the admin API starts, so it is non-nil for any
	// request-triggered pull; shutdown cancels the download.
	ctx := n.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		err := n.computeCap.PullModel(ctx, model, func(p ollama.PullProgress) {
			n.pullMu.Lock()
			n.pullState.Detail = p.Status
			if p.Total > 0 {
				n.pullState.Completed, n.pullState.Total = p.Completed, p.Total
			}
			n.pullMu.Unlock()
		})
		n.pullMu.Lock()
		if err != nil {
			n.pullState.Status = "error"
			n.pullState.Detail = err.Error()
			n.logger.Warn("model pull failed", "model", model, "error", err)
		} else {
			n.pullState.Status = "done"
			n.pullState.Detail = ""
			n.logger.Info("model pull finished", "model", model)
		}
		n.pullMu.Unlock()
	}()
	return nil
}

// adminapi.LogController implementation: the live console.

// LogBacklog implements adminapi.LogController.
func (n *Node) LogBacklog() []logging.Record {
	return n.logs.Backlog()
}

// SubscribeLogs implements adminapi.LogController.
func (n *Node) SubscribeLogs() (<-chan logging.Record, func()) {
	return n.logs.Subscribe(0)
}
