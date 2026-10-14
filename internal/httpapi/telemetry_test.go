package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/StphnLwnga/llm-gateway/internal/provider"
)

type recorded struct {
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
}

// unavailable is a provider whose upstream is down.
type unavailable struct{}

func (unavailable) Chat(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{}, &provider.Error{Provider: "down", Kind: provider.KindUpstreamUnavailable}
}

func (unavailable) Stream(context.Context, provider.Request, func(provider.Event) error) error {
	return &provider.Error{Provider: "down", Kind: provider.KindUpstreamUnavailable}
}

type routes map[string][]Target

func (r routes) Resolve(alias string) []Target { return r[alias] }

func instrumented(t *testing.T) (*Handler, recorded) {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})
	h := New(slog.New(slog.DiscardHandler), Deps{
		Routes:    routes{"down": {{ProviderName: "down", Provider: unavailable{}}}},
		Keys:      FakeKeys{},
		Telemetry: Telemetry{Tracer: tp, Meter: mp, ServerAddr: "llm-gateway:8080"},
	})
	return h, recorded{spans: spans, reader: reader}
}

func attrsOf(kvs []attribute.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

func TestServerSpansNameTheRouteTemplate(t *testing.T) {
	t.Parallel()
	h, rec := instrumented(t)
	cases := []struct {
		method, path        string
		wantName, wantRoute string
		wantStatus          string
	}{
		{http.MethodGet, "/v1/models", "GET /v1/models", "/v1/models", "200"},
		{http.MethodGet, "/v1/nope/req_8f2a91", "GET /", "/", "404"},
		{http.MethodDelete, "/v1/chat/completions", "DELETE /v1/chat/completions", "/v1/chat/completions", "405"},
	}
	for _, tc := range cases {
		h.ServeHTTP(httptest.NewRecorder(), WithKey(httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)))
	}
	ended := rec.spans.Ended()
	if len(ended) != len(cases) {
		t.Fatalf("recorded %d spans, want %d", len(ended), len(cases))
	}
	for i, tc := range cases {
		s := ended[i]
		a := attrsOf(s.Attributes())
		if s.Name() != tc.wantName || a["http.route"] != tc.wantRoute || a["http.response.status_code"] != tc.wantStatus {
			t.Errorf("%s %s: span %q, http.route %q, status %q; want %q, %q, %s",
				tc.method, tc.path, s.Name(), a["http.route"], a["http.response.status_code"], tc.wantName, tc.wantRoute, tc.wantStatus)
		}
	}
}

func TestAServerErrorMarksTheSpanButAClientErrorDoesNot(t *testing.T) {
	t.Parallel()
	h, rec := instrumented(t)
	h.ServeHTTP(httptest.NewRecorder(), WithKey(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/nope", nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, WithKey(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"down","messages":[{"role":"user","content":"hi"}]}`))))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("chat with the upstream down = %d, want 502", w.Code)
	}

	ended := rec.spans.Ended()
	if len(ended) != 2 {
		t.Fatalf("recorded %d spans, want 2", len(ended))
	}
	if got := ended[0].Status().Code; got != codes.Unset {
		t.Errorf("404 span status = %v, want Unset: a client error is not a server fault", got)
	}
	if got := ended[1].Status().Code; got != codes.Error {
		t.Errorf("502 span status = %v, want Error", got)
	}
}

func TestServerMetricsCarryTheRouteAndAFixedServerAddress(t *testing.T) {
	t.Parallel()
	h, rec := instrumented(t)
	for _, path := range []string{"/v1/models", "/v1/nope/req_8f2a91", "/v1/nope/req_77c0d3"} {
		r := WithKey(httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		r.Host = "attacker.example:31337"
		h.ServeHTTP(httptest.NewRecorder(), r)
	}

	var rm metricdata.ResourceMetrics
	if err := rec.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var points []metricdata.HistogramDataPoint[float64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			if m.Unit != "s" {
				t.Errorf("http.server.request.duration unit = %q, want s", m.Unit)
			}
			points = append(points, m.Data.(metricdata.Histogram[float64]).DataPoints...)
		}
	}
	counts := map[string]uint64{}
	for _, p := range points {
		a := attrsOf(p.Attributes.ToSlice())
		for k, v := range a {
			if strings.Contains(v, "req_") || strings.Contains(v, "attacker") || strings.Contains(v, "31337") {
				t.Errorf("metric attribute %s = %q comes from the request, want a bounded value", k, v)
			}
		}
		if a["server.address"] != "llm-gateway" || a["server.port"] != "8080" {
			t.Errorf("server.address, server.port = %q, %q; want llm-gateway, 8080", a["server.address"], a["server.port"])
		}
		counts[a["http.route"]+" "+a["http.response.status_code"]] += p.Count
	}
	want := map[string]uint64{"/v1/models 200": 1, "/ 404": 2}
	if len(counts) != len(want) || counts["/v1/models 200"] != 1 || counts["/ 404"] != 2 {
		t.Errorf("request counts by route and status = %v, want %v", counts, want)
	}
}

func TestTheInstrumentedWriterKeepsWriteDeadlinesAndFlush(t *testing.T) {
	t.Parallel()
	errs := make(chan [2]error, 1)
	probe := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		errs <- [2]error{rc.SetWriteDeadline(time.Now().Add(time.Second)), rc.Flush()}
	})
	srv := httptest.NewServer(instrument(probe, Telemetry{}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got := <-errs
	if got[0] != nil || got[1] != nil {
		t.Errorf("through the instrumentation, SetWriteDeadline = %v and Flush = %v; want both nil (streams set a deadline per event)", got[0], got[1])
	}
}

func TestProbesRecordMetricsButNoSpans(t *testing.T) {
	t.Parallel()
	h, rec := instrumented(t)
	get := func(path string) {
		h.ServeHTTP(httptest.NewRecorder(), WithKey(httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)))
	}
	get("/healthz")
	get("/readyz")
	h.StartDraining()
	get("/readyz")
	get("/v1/models")

	var names []string
	for _, s := range rec.spans.Ended() {
		names = append(names, s.Name())
	}
	if len(names) != 1 || names[0] != "GET /v1/models" {
		t.Errorf("spans = %q, want only GET /v1/models: probes record no spans", names)
	}

	var rm metricdata.ResourceMetrics
	if err := rec.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	counts := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			for _, p := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				a := attrsOf(p.Attributes.ToSlice())
				counts[a["http.route"]+" "+a["http.response.status_code"]] += p.Count
			}
		}
	}
	want := map[string]uint64{"/healthz 200": 1, "/readyz 200": 1, "/readyz 503": 1, "/v1/models 200": 1}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("requests %q = %d, want %d (all counts %v)", k, counts[k], v, counts)
		}
	}
}
