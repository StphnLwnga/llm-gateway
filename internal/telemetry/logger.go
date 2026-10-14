// Package telemetry builds the gateway's logger.
package telemetry

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/StphnLwnga/llm-gateway/internal/tenant"
)

// NewLogger returns a logger for level and above: text when format is "text",
// JSON otherwise. A record logged with a context that holds a span carries
// trace_id and span_id, and one with an authenticated caller carries
// tenant_id and key_id.
func NewLogger(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == "text" {
		return slog.New(traceHandler{slog.NewTextHandler(w, opts)})
	}
	return slog.New(traceHandler{slog.NewJSONHandler(w, opts)})
}

type traceHandler struct {
	slog.Handler
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	if id, ok := tenant.FromContext(ctx); ok {
		r.AddAttrs(slog.String("tenant_id", id.TenantID), slog.String("key_id", id.KeyID))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}
