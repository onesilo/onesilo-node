// Package logging builds the process-wide slog.Logger from config.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/onesilo/onesilo-node/internal/config"
)

// New returns a logger writing to stderr in the configured format and level.
func New(cfg config.Log) *slog.Logger {
	return NewWithWriter(cfg, os.Stderr)
}

// NewWithWriter is New with an explicit destination. The setup control
// panel uses it to send node logs to a file so they don't scribble over
// the interactive screen.
func NewWithWriter(cfg config.Log, w io.Writer) *slog.Logger {
	return slog.New(handlerFor(cfg, w, handlerOptions(cfg)))
}

// handlerOptions maps the configured level onto slog's.
func handlerOptions(cfg config.Log) *slog.HandlerOptions {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return &slog.HandlerOptions{Level: level}
}

// handlerFor builds the configured-format handler over w.
func handlerFor(cfg config.Log, w io.Writer, opts *slog.HandlerOptions) slog.Handler {
	if cfg.Format == "json" {
		return slog.NewJSONHandler(w, opts)
	}
	return slog.NewTextHandler(w, opts)
}
