package otelsdk

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	collmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// otlpServer records every OTLP/HTTP export it receives, by URL path.
type otlpServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies map[string][][]byte
}

func newOTLPServer(t *testing.T) *otlpServer {
	t.Helper()
	s := &otlpServer{bodies: map[string][][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies[r.URL.Path] = append(s.bodies[r.URL.Path], b)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *otlpServer) received(path string) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.bodies[path])
}

func (s *otlpServer) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.bodies {
		n += len(b)
	}
	return n
}

func record(ctx context.Context, p *Providers, spanName string) {
	_, span := p.Tracer.Tracer("test").Start(ctx, spanName)
	span.End()
	counter, _ := p.Meter.Meter("test").Int64Counter("test.requests")
	counter.Add(ctx, 1)
}

func TestNoEndpointGivesNoopProvidersThatSendNothing(t *testing.T) {
	srv := newOTLPServer(t)
	// The exporters would read these; with no endpoint passed, no exporter exists.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)

	p, err := New(t.Context(), "", "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := p.Tracer.(tracenoop.TracerProvider); !ok {
		t.Errorf("Tracer = %T, want a no-op provider", p.Tracer)
	}
	if _, ok := p.Meter.(metricnoop.MeterProvider); !ok {
		t.Errorf("Meter = %T, want a no-op provider", p.Meter)
	}
	record(t.Context(), p, "unexported")
	if err := p.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if n := srv.total(); n != 0 {
		t.Errorf("the OTLP server received %d exports, want 0", n)
	}
}

func TestSpanCarriesTheResource(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		want       map[string]string
		wantAbsent string
	}{
		{
			name:       "defaults",
			want:       map[string]string{"service.name": "llm-gateway", "service.version": "v1.2.3", "telemetry.sdk.language": "go"},
			wantAbsent: "deployment.environment.name",
		},
		{
			name: "OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override and add",
			env: map[string]string{
				"OTEL_SERVICE_NAME":        "gateway-eu",
				"OTEL_RESOURCE_ATTRIBUTES": "deployment.environment.name=staging",
			},
			want: map[string]string{"service.name": "gateway-eu", "service.version": "v1.2.3", "deployment.environment.name": "staging"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OTEL_SERVICE_NAME", "")
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			res, err := newResource(t.Context(), "v1.2.3")
			if err != nil {
				t.Fatalf("newResource: %v", err)
			}
			spans := tracetest.NewInMemoryExporter()
			p := newSDKProviders(res, spans, sdkmetric.NewManualReader())
			t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

			record(t.Context(), p, "with-resource")
			if err := p.Tracer.(*sdktrace.TracerProvider).ForceFlush(t.Context()); err != nil {
				t.Fatalf("ForceFlush: %v", err)
			}
			got := spans.GetSpans()
			if len(got) != 1 {
				t.Fatalf("exported %d spans, want 1", len(got))
			}
			attrs := map[attribute.Key]string{}
			for _, kv := range got[0].Resource.Attributes() {
				attrs[kv.Key] = kv.Value.String()
			}
			for k, v := range tc.want {
				if attrs[attribute.Key(k)] != v {
					t.Errorf("resource %s = %q, want %q", k, attrs[attribute.Key(k)], v)
				}
			}
			if _, ok := attrs[attribute.Key(tc.wantAbsent)]; tc.wantAbsent != "" && ok {
				t.Errorf("resource has %s, want it absent", tc.wantAbsent)
			}
		})
	}
}

func TestSamplerRecordsEverySpanAndKeepsTheIncomingTraceID(t *testing.T) {
	t.Parallel()
	spans := tracetest.NewInMemoryExporter()
	p := newSDKProviders(nil, spans, sdkmetric.NewManualReader())
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	tracer := p.Tracer.Tracer("test")

	_, root := tracer.Start(t.Context(), "root")
	root.End()

	notSampled := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, Remote: true, // TraceFlags 0: not sampled
	})
	_, child := tracer.Start(trace.ContextWithRemoteSpanContext(t.Context(), notSampled), "child")
	recording := child.IsRecording() // false once the span ends
	child.End()

	if !root.SpanContext().IsSampled() {
		t.Error("a root span is not sampled, want every root sampled")
	}
	if !recording || !child.SpanContext().IsSampled() {
		t.Error("the child of a remote parent with flag 00 is not recorded and sampled, want the collector to decide")
	}
	if child.SpanContext().TraceID() != notSampled.TraceID() {
		t.Errorf("child trace ID = %s, want the incoming %s", child.SpanContext().TraceID(), notSampled.TraceID())
	}
}

func TestShutdownFlushesToTheOTLPEndpoint(t *testing.T) {
	srv := newOTLPServer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)

	p, err := New(t.Context(), srv.URL, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	record(t.Context(), p, "flushed-on-shutdown")
	if n := srv.total(); n != 0 {
		t.Fatalf("the OTLP server received %d exports before Shutdown, want 0 (the test would not show the flush)", n)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	var spanNames []string
	for _, b := range srv.received("/v1/traces") {
		var req colltrace.ExportTraceServiceRequest
		if err := proto.Unmarshal(b, &req); err != nil {
			t.Fatalf("decode trace export: %v", err)
		}
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, s := range ss.GetSpans() {
					spanNames = append(spanNames, s.GetName())
				}
			}
		}
	}
	if !slices.Contains(spanNames, "flushed-on-shutdown") {
		t.Errorf("trace exports hold spans %q, want flushed-on-shutdown", spanNames)
	}

	var metricNames []string
	for _, b := range srv.received("/v1/metrics") {
		var req collmetrics.ExportMetricsServiceRequest
		if err := proto.Unmarshal(b, &req); err != nil {
			t.Fatalf("decode metric export: %v", err)
		}
		for _, rm := range req.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					metricNames = append(metricNames, m.GetName())
				}
			}
		}
	}
	if !slices.Contains(metricNames, "test.requests") {
		t.Errorf("metric exports hold %q, want test.requests", metricNames)
	}
}
