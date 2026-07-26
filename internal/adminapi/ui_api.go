package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
)

// ErrPullInProgress is returned by UIController.StartPull when a pull is
// already running; the route maps it to 409 Conflict. Other StartPull
// errors are treated as unavailability (503), not conflicts.
var ErrPullInProgress = errors.New("a model pull is already in progress")

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
		// silo_id is client-controlled (any string a memory was written
		// under), so never concatenate it into the header raw. mime encodes
		// a safe RFC 2231 filename*; a sanitized ASCII fallback covers old
		// clients. This closes off Content-Disposition header injection.
		w.Header().Set("Content-Disposition", contentDisposition(siloID+".silo"))
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
			// Only an in-progress pull is a genuine conflict; everything else
			// (compute disabled, etc.) is an availability problem.
			code := http.StatusServiceUnavailable
			if errors.Is(err, ErrPullInProgress) {
				code = http.StatusConflict
			}
			writeError(w, code, err.Error())
			return
		}
		logger.Info("model pull started via admin API", "model", body.Model)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pulling", "model": body.Model})
	})
}

// contentDisposition builds an attachment Content-Disposition value with a
// sanitized ASCII fallback filename plus an RFC 2231 filename* for the
// exact name — never interpolating the raw (client-controlled) name into
// the header.
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, name)
	if ascii == "" {
		ascii = "silo.silo"
	}
	return mime.FormatMediaType("attachment", map[string]string{"filename": ascii}) +
		"; " + "filename*=UTF-8''" + urlEncodePath(name)
}

// urlEncodePath percent-encodes per RFC 5987 (attr-char set) for filename*.
func urlEncodePath(s string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0f])
		}
	}
	return b.String()
}
