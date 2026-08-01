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

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler)
}
