package httpapi

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Probes get metrics but no spans (ADR 0016): a probe trace has no debugging
// value, and a failing readiness check must still show in the metrics.
var untracedPaths = map[string]bool{"/healthz": true, "/readyz": true}

// Telemetry records the server spans and metrics. Nil providers and a nil
// propagator use the global ones.
type Telemetry struct {
	Tracer trace.TracerProvider
	Meter  metric.MeterProvider
	// Propagator reads an incoming traceparent, so the server span continues the client's trace.
	Propagator propagation.TextMapPropagator
	// ServerAddr ("host:port") is the server.address and server.port of every
	// metric. When it is empty, they come from the request's Host header.
	ServerAddr string
}

// instrument records one server span and the http.server.* metrics for each
// request, and only the metrics for a probe. The route is the ServeMux
// pattern that matched, never the raw path.
// mux must be a *http.ServeMux, which sets the request's Pattern.
func instrument(mux http.Handler, t Telemetry) http.Handler {
	var opts []otelhttp.Option
	// A client chooses the Host header; without a fixed name, every value
	// would be a new metric series.
	if t.ServerAddr != "" {
		opts = append(opts, otelhttp.WithServerName(t.ServerAddr))
	}
	if t.Tracer != nil {
		opts = append(opts, otelhttp.WithTracerProvider(t.Tracer))
	}
	if t.Meter != nil {
		opts = append(opts, otelhttp.WithMeterProvider(t.Meter))
	}
	if t.Propagator != nil {
		opts = append(opts, otelhttp.WithPropagators(t.Propagator))
	}
	// otelhttp names the span from the pattern but does not add http.route to
	// it; the pattern is known only after the mux has matched.
	withRoute := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if i := strings.IndexByte(r.Pattern, '/'); i >= 0 {
			trace.SpanFromContext(r.Context()).SetAttributes(semconv.HTTPRoute(r.Pattern[i:]))
		}
	})
	traced := otelhttp.NewHandler(withRoute, "http.server", opts...)
	// otelhttp's filter would drop the metrics too, so probes go through a
	// second handler whose tracer records nothing.
	untraced := otelhttp.NewHandler(withRoute, "http.server",
		append(opts, otelhttp.WithTracerProvider(tracenoop.NewTracerProvider()))...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if untracedPaths[r.URL.Path] {
			untraced.ServeHTTP(w, r)
			return
		}
		traced.ServeHTTP(w, r)
	})
}
