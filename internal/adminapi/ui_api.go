package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// Types and routes backing the embedded admin UI (Silos / Models /
// Settings). Kept in adminapi's own vocabulary so the package stays
// decoupled from internal/memory and internal/compute — the node adapts.

// SiloInfo is one silo row for GET /v1/silos.
type SiloInfo struct {
	SiloID string `json:"silo_id"`
	Count  int    `json:"count"`
}

// MemoryRecord is one memory for GET /v1/silos/{silo_id}/memories.
type MemoryRecord struct {
	ID        string         `json:"id"`
	Content   string         `json:"content"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

// ModelInfo is one installed Ollama model.
type ModelInfo struct {
	Name       string `json:"name"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	ModifiedAt string `json:"modified_at,omitempty"`
	Active     bool   `json:"active"`
	Default    bool   `json:"default"`
}

// PullState reports a background model pull for GET /v1/models.
type PullState struct {
	Model     string `json:"model"`
	Status    string `json:"status"` // "pulling" | "done" | "error"
	Detail    string `json:"detail,omitempty"`
	Completed int64  `json:"completed,omitempty"`
	Total     int64  `json:"total,omitempty"`
}

// ModelsInfo is the GET /v1/models response.
type ModelsInfo struct {
	// Active is the model the compute capability currently serves.
	Active string `json:"active"`
	// Default is ollama.default_model from config (what "activate" sets).
	Default string      `json:"default"`
	Models  []ModelInfo `json:"models"`
	Pull    *PullState  `json:"pull,omitempty"`
	// Error is a non-fatal listing problem (e.g. Ollama unreachable).
	Error string `json:"error,omitempty"`
}

// UIController is the extra surface the admin UI needs. Implemented by
// *node.Node alongside Controller.
type UIController interface {
	ListSilos(ctx context.Context) ([]SiloInfo, error)
	ListMemories(ctx context.Context, siloID string) ([]MemoryRecord, error)
	ForgetMemory(ctx context.Context, siloID, memoryID string) (bool, error)
	// ExportSilo writes the silo as a .silo package to w.
	ExportSilo(ctx context.Context, siloID string, w io.Writer) error
	Models(ctx context.Context) ModelsInfo
	// StartPull begins a background model pull; progress via Models().
	StartPull(model string) error
}

// registerUIRoutes mounts the UI-backing routes when the controller also
// implements UIController.
func registerUIRoutes(authed func(string, http.HandlerFunc), ctrl Controller, logger *slog.Logger) {
	ui, ok := ctrl.(UIController)
	if !ok {
		return
	}

	authed("GET /v1/silos", func(w http.ResponseWriter, r *http.Request) {
		silos, err := ui.ListSilos(r.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if silos == nil {
			silos = []SiloInfo{}
		}
		writeJSON(w, http.StatusOK, silos)
	})

	authed("GET /v1/silos/{silo_id}/memories", func(w http.ResponseWriter, r *http.Request) {
		items, err := ui.ListMemories(r.Context(), r.PathValue("silo_id"))
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if items == nil {
			items = []MemoryRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"memories": items})
	})

	authed("DELETE /v1/silos/{silo_id}/memories/{memory_id}", func(w http.ResponseWriter, r *http.Request) {
		deleted, err := ui.ForgetMemory(r.Context(), r.PathValue("silo_id"), r.PathValue("memory_id"))
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if !deleted {
			writeError(w, http.StatusNotFound, "memory not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	})

	authed("GET /v1/silos/{silo_id}/export", func(w http.ResponseWriter, r *http.Request) {
		siloID := r.PathValue("silo_id")
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+siloID+`.silo"`)
		if err := ui.ExportSilo(r.Context(), siloID, w); err != nil {
			// Headers may already be gone; log and cut the stream.
			logger.Warn("silo export failed", "silo", siloID, "error", err)
		}
	})

	authed("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ui.Models(r.Context()))
	})

	authed("POST /v1/models/pull", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model == "" {
			writeError(w, http.StatusBadRequest, "expected JSON body {\"model\": \"...\"}")
			return
		}
		if err := ui.StartPull(body.Model); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		logger.Info("model pull started via admin API", "model", body.Model)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pulling", "model": body.Model})
	})
}
