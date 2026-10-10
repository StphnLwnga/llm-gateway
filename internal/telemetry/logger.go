// Package telemetry builds the gateway's logger. Traces and metrics arrive in S2.
package telemetry

import (
	"io"
	"log/slog"
)

// NewLogger returns a structured logger that writes records at level and above to w.
// format "text" selects key=value lines; any other value selects one JSON object per line.
func NewLogger(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
