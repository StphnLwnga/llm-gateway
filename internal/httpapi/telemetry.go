package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/StphnLwnga/llm-gateway/internal/provider"
)

const scope = "github.com/StphnLwnga/llm-gateway/internal/httpapi"

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

// chatMetrics measure chat requests as the client sees them. Each provider
// call has its own GenAI metrics in the provider package.
type chatMetrics struct {
	commits, truncations, disconnects metric.Int64Counter
	firstToken                        metric.Float64Histogram
}

func newChatMetrics(mp metric.MeterProvider) (chatMetrics, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	m := mp.Meter(scope)
	var c chatMetrics
	var errs [4]error
	c.commits, errs[0] = m.Int64Counter("llm_gateway.chat.stream.commits", metric.WithUnit("{stream}"),
		metric.WithDescription("Streamed requests, by what sent the status: the first token, the end of the hold, or an error."))
	c.truncations, errs[1] = m.Int64Counter("llm_gateway.chat.stream.truncations", metric.WithUnit("{stream}"),
		metric.WithDescription("Streams cut by an upstream error after the status 200 was sent."))
	c.disconnects, errs[2] = m.Int64Counter("llm_gateway.chat.client_disconnects", metric.WithUnit("{request}"),
		metric.WithDescription("Requests whose client left before the answer ended."))
	c.firstToken, errs[3] = m.Float64Histogram("llm_gateway.chat.time_to_first_token", metric.WithUnit("s"),
		metric.WithDescription("Time from the request's arrival to the first content written to the client."),
		metric.WithExplicitBucketBoundaries(provider.DurationBuckets()...))
	return c, errors.Join(errs[:]...)
}

// recordChat counts a request by how it ended, never by its status: a stream
// that sent 200 can still fail.
func (h *Handler) recordChat(ctx context.Context, rec *chatLog) {
	if rec.stream && rec.committedBy != "" {
		h.metrics.commits.Add(ctx, 1, metric.WithAttributes(attribute.String("llm_gateway.commit.reason", rec.committedBy)))
	}
	if rec.truncated {
		h.metrics.truncations.Add(ctx, 1, metric.WithAttributes(
			attribute.String("gen_ai.provider.name", rec.provider), semconv.ErrorTypeKey.String(rec.errType)))
	}
	if rec.clientGone {
		stage := "before_commit"
		if rec.committedBy != "" {
			stage = "after_commit"
		}
		h.metrics.disconnects.Add(ctx, 1, metric.WithAttributes(attribute.String("llm_gateway.stage", stage)))
	}
	if rec.firstVisible > 0 {
		h.metrics.firstToken.Record(ctx, rec.firstVisible.Seconds(), metric.WithAttributes(attribute.String("gen_ai.provider.name", rec.provider)))
	}
}
