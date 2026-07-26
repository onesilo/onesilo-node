package adminapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/onesilo/silo-node/internal/adminui"
	"github.com/onesilo/silo-node/internal/config"
)

// ConfigPatch is the PUT /v1/config body: a partial update where only
// present fields are applied. Mirrors config.Config with pointer fields.
type ConfigPatch struct {
	Mode         *string            `json:"mode,omitempty"`
	DataDir      *string            `json:"data_dir,omitempty"`
	Log          *LogPatch          `json:"log,omitempty"`
	Capabilities *CapabilitiesPatch `json:"capabilities,omitempty"`
	ControlPlane *ControlPlanePatch `json:"control_plane,omitempty"`
	Memory       *MemoryPatch       `json:"memory,omitempty"`
	Ollama       *OllamaPatch       `json:"ollama,omitempty"`
	Tunnel       *TunnelPatch       `json:"tunnel,omitempty"`
	LAN          *LANPatch          `json:"lan,omitempty"`
	Admin        *AdminPatch        `json:"admin,omitempty"`
}

type LogPatch struct {
	Format *string `json:"format,omitempty"`
	Level  *string `json:"level,omitempty"`
}

type CapabilitiesPatch struct {
	Memory  *bool `json:"memory,omitempty"`
	Compute *bool `json:"compute,omitempty"`
}

type ControlPlanePatch struct {
	URL        *string `json:"url,omitempty"`
	AuthMode   *string `json:"auth_mode,omitempty"`
	DeviceName *string `json:"device_name,omitempty"`
}

type MemoryPatch struct {
	EmbedModel *string `json:"embed_model,omitempty"`
}

type OllamaPatch struct {
	Host         *string `json:"host,omitempty"`
	Manage       *bool   `json:"manage,omitempty"`
	DefaultModel *string `json:"default_model,omitempty"`
	BinaryPath   *string `json:"binary_path,omitempty"`
}

type TunnelPatch struct {
	Mode            *string `json:"mode,omitempty"`
	CloudflaredPath *string `json:"cloudflared_path,omitempty"`
	ExternalURL     *string `json:"external_url,omitempty"`
}

type LANPatch struct {
	Enabled *bool `json:"enabled,omitempty"`
	Port    *int  `json:"port,omitempty"`
}

type AdminPatch struct {
	Port *int `json:"port,omitempty"`
}

func setIf[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// ApplyTo merges the patch into cfg (present fields only).
func (p ConfigPatch) ApplyTo(cfg *config.Config) {
	setIf(&cfg.Mode, p.Mode)
	setIf(&cfg.DataDir, p.DataDir)
	if p.Log != nil {
		setIf(&cfg.Log.Format, p.Log.Format)
		setIf(&cfg.Log.Level, p.Log.Level)
	}
	if p.Capabilities != nil {
		setIf(&cfg.Capabilities.Memory, p.Capabilities.Memory)
		setIf(&cfg.Capabilities.Compute, p.Capabilities.Compute)
	}
	if p.ControlPlane != nil {
		setIf(&cfg.ControlPlane.URL, p.ControlPlane.URL)
		setIf(&cfg.ControlPlane.AuthMode, p.ControlPlane.AuthMode)
		setIf(&cfg.ControlPlane.DeviceName, p.ControlPlane.DeviceName)
	}
	if p.Memory != nil {
		setIf(&cfg.Memory.EmbedModel, p.Memory.EmbedModel)
	}
	if p.Ollama != nil {
		setIf(&cfg.Ollama.Host, p.Ollama.Host)
		setIf(&cfg.Ollama.Manage, p.Ollama.Manage)
		setIf(&cfg.Ollama.DefaultModel, p.Ollama.DefaultModel)
		setIf(&cfg.Ollama.BinaryPath, p.Ollama.BinaryPath)
	}
	if p.Tunnel != nil {
		setIf(&cfg.Tunnel.Mode, p.Tunnel.Mode)
		setIf(&cfg.Tunnel.CloudflaredPath, p.Tunnel.CloudflaredPath)
		setIf(&cfg.Tunnel.ExternalURL, p.Tunnel.ExternalURL)
	}
	if p.LAN != nil {
		setIf(&cfg.LAN.Enabled, p.LAN.Enabled)
		setIf(&cfg.LAN.Port, p.LAN.Port)
	}
	if p.Admin != nil {
		setIf(&cfg.Admin.Port, p.Admin.Port)
	}
}

// pairingKeyPattern validates the legacy LAN pairing key format.
var pairingKeyPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func newMux(adminToken string, ctrl Controller, logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()

	// Liveness probe: no auth, used by `silo-node healthcheck` and Docker.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	// The embedded admin UI. Static assets are served without auth (the
	// server binds loopback only); every API call the UI makes still
	// carries the bearer token.
	mux.Handle("GET /", adminui.Handler())

	authed := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, requireAdminToken(adminToken, h))
	}

	registerUIRoutes(authed, ctrl, logger)

	authed("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ctrl.Status(r.Context()))
	})

	authed("GET /v1/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ctrl.ConfigSnapshot())
	})

	authed("PUT /v1/config", func(w http.ResponseWriter, r *http.Request) {
		var patch ConfigPatch
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&patch); err != nil {
			writeError(w, http.StatusBadRequest, "invalid config patch: "+err.Error())
			return
		}
		cfg, err := ctrl.ApplyConfigPatch(r.Context(), patch)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		logger.Info("config updated via admin API")
		writeJSON(w, http.StatusOK, cfg)
	})

	authed("POST /v1/auth/jwt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
			writeError(w, http.StatusBadRequest, "expected JSON body {\"token\": \"<jwt>\"}")
			return
		}
		ctrl.SetJWT(body.Token)
		logger.Info("control-plane JWT updated via admin API")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	authed("POST /v1/auth/pairing-key", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PairingKey string `json:"pairing_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "expected JSON body {\"pairing_key\": \"<64 hex chars>\"}")
			return
		}
		if !pairingKeyPattern.MatchString(body.PairingKey) {
			writeError(w, http.StatusBadRequest, "pairing_key must be exactly 64 hex characters")
			return
		}
		if err := ctrl.SetPairingKey(body.PairingKey); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		logger.Info("pairing key updated via admin API")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	authed("POST /v1/compute/generate", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt      string   `json:"prompt"`
			Temperature *float64 `json:"temperature"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil || body.Prompt == "" {
			writeError(w, http.StatusBadRequest,
				"expected JSON body {\"prompt\": \"...\", \"temperature\": 0.2?}")
			return
		}
		// Exactly one JSON object: trailing values or garbage are malformed.
		if err := dec.Decode(new(struct{})); err != io.EOF {
			writeError(w, http.StatusBadRequest, "request body must be a single JSON object")
			return
		}
		// Low default: local callers (e.g. a Buzz agent distilling
		// conversation privately) want extraction, not creativity.
		temperature := 0.2
		if body.Temperature != nil {
			temperature = *body.Temperature
		}
		text, model, err := ctrl.Generate(r.Context(), body.Prompt, temperature)
		if err != nil {
			// Compute down / Ollama unreachable — the caller should retry
			// later rather than treat this as a bad request.
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"text": text, "model": model})
	})

	authed("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		logger.Info("shutdown requested via admin API")
		writeJSON(w, http.StatusAccepted, map[string]bool{"shutting_down": true})
		// Trigger after responding so the client gets its ack.
		go ctrl.Shutdown()
	})

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
