package provider

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// The GenAI names follow open-telemetry/semantic-conventions-genai at commit
// 6fd0d763a092db245a54104fc1124031eb3c51d2 (ADR 0016). They are in
// development, and the Go semconv packages do not have them.
const (
	scope = "github.com/StphnLwnga/llm-gateway/internal/provider"

	operationChat = "chat"

	attrOperationName    = "gen_ai.operation.name"
	attrProviderName     = "gen_ai.provider.name"
	attrRequestModel     = "gen_ai.request.model"
	attrRequestStream    = "gen_ai.request.stream"
	attrResponseModel    = "gen_ai.response.model"
	attrFinishReasons    = "gen_ai.response.finish_reasons"
	attrTimeToFirstChunk = "gen_ai.response.time_to_first_chunk"
	attrUsageIn          = "gen_ai.usage.input_tokens"
	attrUsageOut         = "gen_ai.usage.output_tokens"
	attrModality         = "gen_ai.token.modality"
	attrDeadline         = "llm_gateway.deadline"

	// The provider's own timings, in seconds. The GenAI registry has no names
	// for them. They go on the span only, so they add no series.
	attrQueueDuration   = "llm_gateway.provider.queue_duration"
	attrLoadDuration    = "llm_gateway.provider.load_duration"
	attrPrefillDuration = "llm_gateway.provider.prefill_duration"
	attrDecodeDuration  = "llm_gateway.provider.decode_duration"
	attrTotalDuration   = "llm_gateway.provider.total_duration"

	// ErrorTypeCancelled is the error.type of a call whose caller gave up.
	ErrorTypeCancelled = "cancelled"

	// maxResponseModel caps the one value an upstream chooses.
	maxResponseModel = 256
)

// DurationBuckets returns the bucket boundaries, in seconds, that the GenAI
// conventions advise for every duration histogram.
func DurationBuckets() []float64 {
	return []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
}

// ErrorType is the error.type of a failed call: the *Error's kind, or _OTHER.
func ErrorType(err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return string(pe.Kind)
	}
	return semconv.ErrorTypeOther.Value.AsString()
}

// GenAI records one gen_ai.client.inference span and the GenAI client
// inference metrics for each call. It never records prompt or completion
// text. Wrap it around the Watchdog, so a passed deadline ends the span.
type GenAI struct {
	inner  Provider
	name   string
	server []attribute.KeyValue
	tracer trace.Tracer

	duration, firstChunk, perChunk metric.Float64Histogram
	inputTokens, outputTokens      metric.Int64Counter
	timeouts                       metric.Int64Counter
	firstTokenTimeout, stall       attribute.Set
}

// NewGenAI wraps inner, the provider called name at baseURL. Nil providers
// use the global ones.
func NewGenAI(inner Provider, name, baseURL string, tp trace.TracerProvider, mp metric.MeterProvider) (*GenAI, error) {
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	m := mp.Meter(scope)
	g := &GenAI{inner: inner, name: name, server: serverAttrs(baseURL), tracer: tp.Tracer(scope)}
	buckets := metric.WithExplicitBucketBoundaries(DurationBuckets()...)
	var errs [6]error
	g.duration, errs[0] = m.Float64Histogram("gen_ai.client.inference.duration",
		metric.WithUnit("s"), metric.WithDescription("GenAI client inference operation duration."), buckets)
	g.firstChunk, errs[1] = m.Float64Histogram("gen_ai.client.inference.time_to_first_chunk",
		metric.WithUnit("s"), metric.WithDescription("Time from the request to the first chunk of the response stream."), buckets)
	g.perChunk, errs[2] = m.Float64Histogram("gen_ai.client.inference.time_per_output_chunk",
		metric.WithUnit("s"), metric.WithDescription("Time per output chunk after the first."), buckets)
	g.inputTokens, errs[3] = m.Int64Counter("gen_ai.client.inference.usage.input_tokens",
		metric.WithUnit("{token}"), metric.WithDescription("The number of input (prompt) tokens used."))
	g.outputTokens, errs[4] = m.Int64Counter("gen_ai.client.inference.usage.output_tokens",
		metric.WithUnit("{token}"), metric.WithDescription("The number of output (completion) tokens used."))
	g.timeouts, errs[5] = m.Int64Counter("llm_gateway.provider.watchdog.timeouts",
		metric.WithUnit("{timeout}"), metric.WithDescription("Provider calls ended by a liveness deadline."))
	named := attribute.String(attrProviderName, name)
	g.firstTokenTimeout = attribute.NewSet(named, attribute.String(attrDeadline, "first_token"))
	g.stall = attribute.NewSet(named, attribute.String(attrDeadline, "stall"))
	return g, errors.Join(errs[:]...)
}

// serverAttrs gives server.address and server.port, from configuration and
// never from a request, so they stay bounded.
func serverAttrs(baseURL string) []attribute.KeyValue {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		port = map[string]int{"http": 80, "https": 443}[u.Scheme]
	}
	return []attribute.KeyValue{semconv.ServerAddress(u.Hostname()), semconv.ServerPort(port)}
}

// Chat records a non-streamed call: no chunk timings.
func (g *GenAI) Chat(ctx context.Context, req Request) (Response, error) {
	c := g.start(ctx, req, false)
	resp, err := g.inner.Chat(c.ctx, req)
	if err == nil {
		c.model, c.finish, c.usage = resp.Model, resp.FinishReason, &resp.Usage
	}
	c.end(ctx, err)
	return resp, err
}

// Stream records the chunk timings as well. A chunk is an event with text or
// reasoning. The time fn takes to hand a chunk on is not upstream time, so it
// is left out of the next chunk's gap, as the Watchdog does.
func (g *GenAI) Stream(ctx context.Context, req Request, fn func(Event) error) error {
	c := g.start(ctx, req, true)
	var handedOn time.Time // after the previous chunk; zero before the first
	err := g.inner.Stream(c.ctx, req, func(e Event) error {
		now := time.Now()
		if c.model == "" {
			c.model = e.Model
		}
		if e.FinishReason != "" {
			c.finish = e.FinishReason
		}
		if e.Usage != nil {
			c.usage = e.Usage
		}
		chunk := e.Delta != "" || e.Progress
		if chunk && handedOn.IsZero() {
			c.ttfc, c.chunked = now.Sub(c.began), true
			g.firstChunk.Record(c.ctx, c.ttfc.Seconds(), c.shared)
		} else if chunk {
			g.perChunk.Record(c.ctx, now.Sub(handedOn).Seconds(), c.shared)
		}
		ferr := fn(e)
		if chunk {
			handedOn = time.Now()
		}
		return ferr
	})
	c.end(ctx, err)
	return err
}

// call is one provider call being recorded.
type call struct {
	g      *GenAI
	ctx    context.Context // carries the span, so the HTTP client span is its child
	span   trace.Span
	began  time.Time
	attrs  []attribute.KeyValue     // the metric attributes of every record
	shared metric.MeasurementOption // attrs as one set, so a record per chunk allocates nothing

	ttfc          time.Duration
	chunked       bool // a chunk arrived, so ttfc is set
	model, finish string
	usage         *Usage
}

func (g *GenAI) start(ctx context.Context, req Request, stream bool) *call {
	attrs := slices.Concat([]attribute.KeyValue{
		attribute.String(attrOperationName, operationChat),
		attribute.String(attrProviderName, g.name),
		attribute.String(attrRequestModel, req.Model),
	}, g.server)
	spanAttrs := slices.Clone(attrs)
	if stream {
		spanAttrs = append(spanAttrs, attribute.Bool(attrRequestStream, true))
	}
	began := time.Now()
	// The span and its attributes exist before the call, so a sampler can use them.
	ctx, span := g.tracer.Start(ctx, operationChat+" "+req.Model, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(began), trace.WithAttributes(spanAttrs...))
	return &call{g: g, ctx: ctx, span: span, began: began, attrs: attrs,
		shared: metric.WithAttributeSet(attribute.NewSet(slices.Clone(attrs)...))}
}

// end closes the span and records the duration and the usage. caller is the
// context the caller passed, which tells a cancel from an upstream failure.
func (c *call) end(caller context.Context, err error) {
	g := c.g
	errType := ""
	if err != nil {
		errType = ErrorType(err)
		if caller.Err() != nil {
			errType = ErrorTypeCancelled
		}
	}
	switch {
	case errType == ErrorTypeCancelled:
	case errors.Is(err, ErrFirstTokenTimeout):
		g.timeouts.Add(c.ctx, 1, metric.WithAttributeSet(g.firstTokenTimeout))
	case errors.Is(err, ErrStallTimeout):
		g.timeouts.Add(c.ctx, 1, metric.WithAttributeSet(g.stall))
	}

	if c.model != "" {
		c.span.SetAttributes(attribute.String(attrResponseModel, truncate(c.model, maxResponseModel)))
	}
	if c.chunked {
		c.span.SetAttributes(attribute.Float64(attrTimeToFirstChunk, c.ttfc.Seconds()))
	}
	switch {
	case err != nil:
		// The answer was expected but did not finish, so its position reports error.
		c.span.SetAttributes(attribute.StringSlice(attrFinishReasons, []string{"error"}))
	case c.finish != "":
		c.span.SetAttributes(attribute.StringSlice(attrFinishReasons, []string{c.finish}))
	}
	if c.usage != nil {
		c.span.SetAttributes(attribute.Int(attrUsageOut, c.usage.CompletionTokens))
		if !c.usage.PromptTokensUnknown {
			c.span.SetAttributes(attribute.Int(attrUsageIn, c.usage.PromptTokens))
		}
		if t := c.usage.Timings; t != nil {
			c.span.SetAttributes(timingAttrs(t)...)
		}
	}
	durationAttrs := c.attrs
	if errType != "" {
		durationAttrs = append(slices.Clone(c.attrs), semconv.ErrorTypeKey.String(errType))
		c.span.SetAttributes(semconv.ErrorTypeKey.String(errType))
		// A cancel is the caller's choice, not a fault. The upstream's message
		// stays out of the span: only the log carries upstream text.
		if errType != ErrorTypeCancelled {
			c.span.SetStatus(codes.Error, errType)
		}
	}

	ended := time.Now()
	c.span.End(trace.WithTimestamp(ended))
	g.duration.Record(c.ctx, ended.Sub(c.began).Seconds(), metric.WithAttributeSet(attribute.NewSet(durationAttrs...)))
	c.recordUsage()
}

// recordUsage counts the tokens of every call, failed ones too: with no usage,
// it records 0 under the unknown modality, as the conventions ask. An unknown
// prompt count is left out rather than counted as 0.
func (c *call) recordUsage() {
	modality, in, out, inKnown := "unknown", 0, 0, true
	if c.usage != nil {
		// The gateway sends text messages only.
		modality, in, out, inKnown = "text", c.usage.PromptTokens, c.usage.CompletionTokens, !c.usage.PromptTokensUnknown
	}
	set := metric.WithAttributeSet(attribute.NewSet(append(slices.Clone(c.attrs), attribute.String(attrModality, modality))...))
	if inKnown {
		c.g.inputTokens.Add(c.ctx, int64(in), set)
	}
	c.g.outputTokens.Add(c.ctx, int64(out), set)
}

// timingAttrs leaves out a timing the provider did not report, because 0 is a
// real value.
func timingAttrs(t *Timings) []attribute.KeyValue {
	var out []attribute.KeyValue
	for _, f := range []struct {
		key string
		d   *time.Duration
	}{
		{attrQueueDuration, t.Queue},
		{attrLoadDuration, t.Load},
		{attrPrefillDuration, t.Prefill},
		{attrDecodeDuration, t.Decode},
		{attrTotalDuration, t.Total},
	} {
		if f.d != nil {
			out = append(out, attribute.Float64(f.key, f.d.Seconds()))
		}
	}
	return out
}

// truncate cuts s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
