package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/StphnLwnga/llm-gateway/internal/fakeupstream"
	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/provider/openaicompat"
	"github.com/StphnLwnga/llm-gateway/internal/telemetry"
	"github.com/StphnLwnga/llm-gateway/internal/telemetry/otelsdk"
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
		Keys:      httpapi.FakeKeys{},
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

func TestAnUnsampledClientTraceIsRecordedAndContinuedUpstream(t *testing.T) {
	t.Parallel()
	exported := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(otelsdk.Sampler()), sdktrace.WithSyncer(exported))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	prop := propagation.TraceContext{}

	upstreamTraceparent := make(chan string, 1)
	fake := fakeupstream.New()
	upstream := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case upstreamTraceparent <- r.Header.Get("traceparent"):
		default:
		}
		fake.ServeHTTP(w, r)
	}))
	client := provider.Instrument(provider.NewHTTPClient(time.Second, 5*time.Second), tp, nil, prop)
	p := openaicompat.New("fake", upstream+"/v1", "test-key", client)
	h := httpapi.New(slog.New(slog.DiscardHandler), httpapi.Deps{
		Routes:    resolver{"chat": {{ProviderName: "fake", Provider: p, UpstreamModel: "fake?status=502"}}},
		Keys:      httpapi.FakeKeys{},
		Chat:      httpapi.ChatConfig{StatusHold: time.Second, HeartbeatInterval: time.Second, MaxBodyBytes: 4 << 10},
		Telemetry: httpapi.Telemetry{Tracer: tp, Propagator: prop},
	})
	url := serve(t, h)

	const clientTraceID, clientSpanID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body(false, "")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-"+clientTraceID+"-"+clientSpanID+"-00") // sampled flag 00
	httpapi.WithKey(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	var server *tracetest.SpanStub
	for _, s := range exported.GetSpans() {
		if s.SpanKind == trace.SpanKindServer {
			server = &s
		}
	}
	if server == nil {
		t.Fatalf("no server span was exported (exported %d spans), want one despite the client's flag 00", len(exported.GetSpans()))
	}
	if got := server.SpanContext.TraceID().String(); got != clientTraceID {
		t.Errorf("server span trace ID = %s, want the client's %s", got, clientTraceID)
	}
	if got := server.Parent.SpanID().String(); got != clientSpanID {
		t.Errorf("server span parent = %s, want the client's span %s", got, clientSpanID)
	}
	if !server.SpanContext.IsSampled() || server.Status.Code != codes.Error {
		t.Errorf("server span sampled %v, status %v; want sampled and Error for a 502", server.SpanContext.IsSampled(), server.Status.Code)
	}

	var got string
	select {
	case got = <-upstreamTraceparent:
	default:
		t.Fatal("the upstream received no request")
	}
	parts := strings.Split(got, "-")
	if len(parts) != 4 || parts[1] != clientTraceID || parts[3] != "01" {
		t.Errorf("upstream traceparent = %q, want trace ID %s and sampled flag 01", got, clientTraceID)
	}
}

func TestTheGenAISpanSitsBetweenTheServerAndHTTPClientSpans(t *testing.T) {
	t.Parallel()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	client := provider.Instrument(provider.NewHTTPClient(time.Second, 5*time.Second), tp, nil, propagation.TraceContext{})
	upstream := serve(t, fakeupstream.New())
	p, err := provider.NewGenAI(openaicompat.New("fake", upstream+"/v1", "test-key", client), "fake", upstream, tp, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.New(slog.New(slog.DiscardHandler), httpapi.Deps{
		Routes:    resolver{"chat": {{ProviderName: "fake", Provider: p, UpstreamModel: "fake"}}},
		Keys:      httpapi.FakeKeys{},
		Chat:      httpapi.ChatConfig{StatusHold: time.Second, HeartbeatInterval: time.Second, MaxBodyBytes: 4 << 10},
		Telemetry: httpapi.Telemetry{Tracer: tp},
	})
	resp := post(t.Context(), t, gateway{url: serve(t, h)}, body(true, ""))
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans.Ended() {
		byName[s.Name()] = s
	}
	server, genai, httpClient := byName["POST /v1/chat/completions"], byName["chat fake"], byName["HTTP POST"]
	if server == nil || genai == nil || httpClient == nil {
		t.Fatalf("spans = %v, want the server, GenAI and HTTP client spans", slices.Collect(maps.Keys(byName)))
	}
	if genai.SpanKind() != trace.SpanKindClient || genai.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Errorf("GenAI span kind %v, parent %v; want client under the server span %v", genai.SpanKind(), genai.Parent().SpanID(), server.SpanContext().SpanID())
	}
	if httpClient.Parent().SpanID() != genai.SpanContext().SpanID() {
		t.Errorf("HTTP client span parent = %v, want the GenAI span %v", httpClient.Parent().SpanID(), genai.SpanContext().SpanID())
	}
}
