package httpapi_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/StphnLwnga/llm-gateway/internal/fakeupstream"
	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/provider/openaicompat"
	"github.com/StphnLwnga/llm-gateway/internal/telemetry"
)

func TestChatLogLinesCarryTheServerSpansTraceID(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			t.Parallel()
			checkChatLogTraceIDs(t, stream)
		})
	}
}

// checkChatLogTraceIDs sends a request whose upstream answers 502, so the
// handler logs the upstream error and then the request line.
func checkChatLogTraceIDs(t *testing.T, stream bool) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	p := openaicompat.New("fake", serve(t, fakeupstream.New())+"/v1", "test-key", provider.NewHTTPClient(time.Second, 5*time.Second))
	logs := &syncBuffer{}
	h := httpapi.New(telemetry.NewLogger(logs, slog.LevelInfo, "json"), httpapi.Deps{
		Routes:    resolver{"chat": {{ProviderName: "fake", Provider: p, UpstreamModel: "fake?status=502"}}},
		Chat:      httpapi.ChatConfig{StatusHold: time.Second, HeartbeatInterval: time.Second, MaxBodyBytes: 4 << 10},
		Telemetry: httpapi.Telemetry{Tracer: tp},
	})
	g := gateway{url: serve(t, h), logs: logs}

	resp := post(t.Context(), t, g, body(stream, ""))
	_ = resp.Body.Close()

	ended := spans.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1 server span", len(ended))
	}
	traceID := ended[0].SpanContext().TraceID().String()
	var msgs []string
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, l)
		}
		msg, _ := rec["msg"].(string)
		msgs = append(msgs, msg)
		if rec["trace_id"] != traceID {
			t.Errorf("log %q has trace_id %v, want the server span's %s", msg, rec["trace_id"], traceID)
		}
	}
	if strings.Join(msgs, ",") != "upstream error,chat completion" {
		t.Errorf("log messages = %q, want the upstream error and the chat completion lines", msgs)
	}
}
