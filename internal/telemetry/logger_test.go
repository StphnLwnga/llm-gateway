package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestLoggerDropsRecordsBelowItsLevel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelWarn, "json")

	log.Debug("debug line")
	log.Info("info line")
	log.Warn("warn line")
	log.Error("error line")

	out := buf.String()
	for _, dropped := range []string{"debug line", "info line"} {
		if strings.Contains(out, dropped) {
			t.Errorf("output contains %q below the warn level:\n%s", dropped, out)
		}
	}
	for _, kept := range []string{"warn line", "error line"} {
		if !strings.Contains(out, kept) {
			t.Errorf("output is missing %q:\n%s", kept, out)
		}
	}
}

func TestJSONLoggerWritesOneObjectPerLine(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo, "json")

	log.Info("first", "port", 8080)
	log.Info("second")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), buf.String())
	}
	var rec struct {
		Level string  `json:"level"`
		Msg   string  `json:"msg"`
		Port  float64 `json:"port"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("line is not JSON: %v\n%s", err, lines[0])
	}
	if rec.Level != "INFO" || rec.Msg != "first" || rec.Port != 8080 {
		t.Errorf("record = %+v, want level INFO, msg first, port 8080", rec)
	}
}

func TestTextLoggerWritesKeyValuePairs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo, "text")

	log.Info("hello", "port", 8080)

	out := buf.String()
	if json.Valid(bytes.TrimSpace(buf.Bytes())) {
		t.Errorf("text format wrote JSON: %s", out)
	}
	for _, want := range []string{"level=INFO", "msg=hello", "port=8080"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing %q", out, want)
		}
	}
}

func TestLogLineInsideASpanCarriesTheTraceAndSpanIDs(t *testing.T) {
	t.Parallel()
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(t.Context(), "request")
	defer span.End()

	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo, "json").With("component", "test")
	log.InfoContext(ctx, "inside")
	log.InfoContext(t.Context(), "outside")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), buf.String())
	}
	var inside, outside map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &inside); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &outside); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	sc := span.SpanContext()
	if inside["trace_id"] != sc.TraceID().String() || inside["span_id"] != sc.SpanID().String() {
		t.Errorf("log inside the span has trace_id %v and span_id %v, want %s and %s",
			inside["trace_id"], inside["span_id"], sc.TraceID(), sc.SpanID())
	}
	if _, ok := outside["trace_id"]; ok {
		t.Errorf("log outside a span has trace_id %v, want none", outside["trace_id"])
	}
}
