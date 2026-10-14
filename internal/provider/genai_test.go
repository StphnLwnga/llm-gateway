package provider

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/StphnLwnga/llm-gateway/internal/tenant"
)

// step is one thing a scripted upstream does after a delay: send an event or fail.
type step struct {
	after time.Duration
	event Event
	err   error
}

type steps []step

func (s steps) Chat(context.Context, Request) (Response, error) {
	return Response{Content: "a reply", FinishReason: "stop", Model: "served-model", Usage: Usage{PromptTokens: 7, CompletionTokens: 2}}, nil
}

func (s steps) Stream(ctx context.Context, _ Request, fn func(Event) error) error {
	for _, st := range s {
		select {
		case <-time.After(st.after):
		case <-ctx.Done():
			return TransportError(ctx, "groq", ctx.Err())
		}
		if st.err != nil {
			return st.err
		}
		if err := fn(st.event); err != nil {
			return err
		}
	}
	return nil
}

// startAttrs keeps each span's attributes as they were when it started.
type startAttrs struct {
	mu  sync.Mutex
	got map[string]map[string]string
}

func (s *startAttrs) OnStart(_ context.Context, sp sdktrace.ReadWriteSpan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got[sp.Name()] = attrMap(sp.Attributes())
}
func (s *startAttrs) OnEnd(sdktrace.ReadOnlySpan)      {}
func (s *startAttrs) Shutdown(context.Context) error   { return nil }
func (s *startAttrs) ForceFlush(context.Context) error { return nil }

type recorded struct {
	spans  *tracetest.SpanRecorder
	starts *startAttrs
	reader *sdkmetric.ManualReader
	tp     *sdktrace.TracerProvider
	g      *GenAI
}

// observe wraps inner as the groq provider and records what it emits.
func observe(t *testing.T, inner Provider) recorded {
	t.Helper()
	r := recorded{spans: tracetest.NewSpanRecorder(), starts: &startAttrs{got: map[string]map[string]string{}}, reader: sdkmetric.NewManualReader()}
	r.tp = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(r.spans), sdktrace.WithSpanProcessor(r.starts))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(r.reader))
	t.Cleanup(func() {
		_ = r.tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})
	g, err := NewGenAI(inner, "groq", "https://api.groq.com/openai/v1", r.tp, mp)
	if err != nil {
		t.Fatal(err)
	}
	r.g = g
	return r
}

func attrMap(kvs []attribute.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

// only returns the one ended span named name.
func (r recorded) only(t *testing.T, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range r.spans.Ended() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("spans named %q = %d, want 1", name, len(found))
	}
	return found[0]
}

func (r recorded) metrics(t *testing.T) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func histogram(t *testing.T, ms map[string]metricdata.Metrics, name string) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	m, ok := ms[name]
	if !ok {
		return nil
	}
	if m.Unit != "s" {
		t.Errorf("%s unit = %q, want s", name, m.Unit)
	}
	return m.Data.(metricdata.Histogram[float64]).DataPoints
}

func counter(ms map[string]metricdata.Metrics, name string) []metricdata.DataPoint[int64] {
	m, ok := ms[name]
	if !ok {
		return nil
	}
	return m.Data.(metricdata.Sum[int64]).DataPoints
}

var chatReq = Request{Model: "llama-3.1-8b-instant", Messages: []Message{{Role: "user", Content: "hi"}}}

const spanName = "chat llama-3.1-8b-instant"

func drain(Event) error { return nil }

func TestTheSpanIsAChatClientSpanUnderTheCallersSpan(t *testing.T) {
	t.Parallel()
	r := observe(t, steps{{after: time.Millisecond, event: Event{Delta: "hello", Model: "served-model"}}})
	ctx, parent := r.tp.Tracer("test").Start(t.Context(), "POST /v1/chat/completions")
	if err := r.g.Stream(ctx, chatReq, drain); err != nil {
		t.Fatal(err)
	}
	parent.End()

	s := r.only(t, spanName)
	if s.SpanKind() != trace.SpanKindClient {
		t.Errorf("kind = %v, want client", s.SpanKind())
	}
	if s.Parent().SpanID() != parent.SpanContext().SpanID() || s.SpanContext().TraceID() != parent.SpanContext().TraceID() {
		t.Errorf("parent = %v, want the caller's span %v", s.Parent().SpanID(), parent.SpanContext().SpanID())
	}
	want := map[string]string{
		"gen_ai.operation.name": "chat", "gen_ai.provider.name": "groq", "gen_ai.request.model": "llama-3.1-8b-instant",
		"server.address": "api.groq.com", "server.port": "443", "gen_ai.request.stream": "true",
	}
	got := r.starts.got[spanName]
	for k, v := range want {
		if got[k] != v {
			t.Errorf("at span start, %s = %q, want %q (a sampler sees only these)", k, got[k], v)
		}
	}
}

func TestAttributesComeFromTheResponse(t *testing.T) {
	t.Parallel()
	u := &Usage{PromptTokens: 12, CompletionTokens: 34}
	r := observe(t, steps{
		{after: time.Millisecond, event: Event{Delta: "hel", Model: "served-model"}},
		{after: time.Millisecond, event: Event{Delta: "lo", Model: "other-model", FinishReason: "length"}},
		{after: time.Millisecond, event: Event{Usage: u}},
	})
	if err := r.g.Stream(t.Context(), chatReq, drain); err != nil {
		t.Fatal(err)
	}
	s := r.only(t, spanName)
	a := attrMap(s.Attributes())
	want := map[string]string{
		"gen_ai.response.model": "served-model", "gen_ai.response.finish_reasons": `["length"]`,
		"gen_ai.usage.input_tokens": "12", "gen_ai.usage.output_tokens": "34",
	}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("%s = %q, want %q", k, a[k], v)
		}
	}
	if _, ok := a["error.type"]; ok || s.Status().Code != codes.Unset {
		t.Errorf("a complete answer has error.type %q and status %v, want neither", a["error.type"], s.Status().Code)
	}

	t.Run("a long response model is cut to 256 bytes", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("m", 255) + "é" // the cut would split é
		r := observe(t, steps{{event: Event{Delta: "x", Model: long}}})
		if err := r.g.Stream(t.Context(), chatReq, drain); err != nil {
			t.Fatal(err)
		}
		got := attrMap(r.only(t, spanName).Attributes())["gen_ai.response.model"]
		if got != strings.Repeat("m", 255) {
			t.Errorf("response model of %d bytes, want the 255 before the split character", len(got))
		}
	})
}

func TestTheResponseModelNeverReachesMetrics(t *testing.T) {
	t.Parallel()
	r := observe(t, steps{
		{after: time.Millisecond, event: Event{Delta: "a", Model: "served-model-2026-10-01"}},
		{after: time.Millisecond, event: Event{Delta: "b", Model: "served-model-2026-10-01"}},
	})
	if err := r.g.Stream(t.Context(), chatReq, drain); err != nil {
		t.Fatal(err)
	}
	for name, m := range r.metrics(t) {
		for _, set := range pointAttrs(m) {
			for k, v := range attrMap(set.ToSlice()) {
				if k == "gen_ai.response.model" || strings.Contains(v, "served-model") {
					t.Errorf("%s carries %s = %q; an upstream chooses that value, so it is unbounded", name, k, v)
				}
			}
		}
	}
}

func TestTheTenantIsOnTheSpanButNeverOnAMetric(t *testing.T) {
	t.Parallel()
	r := observe(t, steps{{after: time.Millisecond, event: Event{Delta: "a", Model: "m"}}})
	ctx := tenant.WithIdentity(t.Context(), tenant.Identity{TenantID: "tenant-7f3e", KeyID: "key-91ab"})
	if err := r.g.Stream(ctx, chatReq, drain); err != nil {
		t.Fatal(err)
	}
	if got := r.starts.got[spanName][tenant.SpanAttribute]; got != "tenant-7f3e" {
		t.Errorf("span starts with %s = %q, want tenant-7f3e", tenant.SpanAttribute, got)
	}
	for name, m := range r.metrics(t) {
		for _, set := range pointAttrs(m) {
			for k, v := range attrMap(set.ToSlice()) {
				if strings.Contains(k, "tenant") || strings.Contains(v, "tenant-7f3e") || strings.Contains(v, "key-91ab") {
					t.Errorf("%s carries %s = %q; a tenant on a metric is a new series per tenant", name, k, v)
				}
			}
		}
	}
}

func pointAttrs(m metricdata.Metrics) []attribute.Set {
	var out []attribute.Set
	switch d := m.Data.(type) {
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes)
		}
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes)
		}
	}
	return out
}

func TestNoPromptOrCompletionTextIsRecorded(t *testing.T) {
	t.Parallel()
	const prompt, reply, system = "prompt-7f3a", "reply-9c1e", "system-41bd"
	r := observe(t, steps{
		{after: time.Millisecond, event: Event{Progress: true}},
		{after: time.Millisecond, event: Event{Delta: reply, FinishReason: "stop"}},
	})
	req := chatReq
	req.Messages = []Message{{Role: "system", Content: system}, {Role: "user", Content: prompt}}
	if err := r.g.Stream(t.Context(), req, drain); err != nil {
		t.Fatal(err)
	}
	s := r.only(t, spanName)
	if len(s.Events()) != 0 {
		t.Errorf("span has %d events, want none: the details event would carry content", len(s.Events()))
	}
	for k, v := range attrMap(s.Attributes()) {
		for _, text := range []string{prompt, reply, system} {
			if strings.Contains(v, text) {
				t.Errorf("span attribute %s carries the text %q", k, text)
			}
		}
	}
}

func TestDurationEqualsTheSpanAndUsesTheAdvisedBuckets(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := observe(t, steps{
			{after: 300 * time.Millisecond, event: Event{Delta: "a"}},
			{after: 200 * time.Millisecond, event: Event{Delta: "b", FinishReason: "stop"}},
		})
		if err := r.g.Stream(t.Context(), chatReq, drain); err != nil {
			t.Fatal(err)
		}
		s := r.only(t, spanName)
		ms := r.metrics(t)
		d := histogram(t, ms, "gen_ai.client.inference.duration")
		if len(d) != 1 || d[0].Count != 1 {
			t.Fatalf("duration points = %+v, want one call", d)
		}
		if spanSecs := s.EndTime().Sub(s.StartTime()).Seconds(); d[0].Sum != spanSecs || spanSecs != 0.5 {
			t.Errorf("duration = %v s, span = %v s; want both 0.5", d[0].Sum, spanSecs)
		}
		for _, name := range []string{"gen_ai.client.inference.duration", "gen_ai.client.inference.time_to_first_chunk", "gen_ai.client.inference.time_per_output_chunk"} {
			ps := histogram(t, ms, name)
			if len(ps) == 0 || !slices.Equal(ps[0].Bounds, DurationBuckets()) {
				t.Errorf("%s bounds = %v, want the advised %v", name, boundsOf(ps), DurationBuckets())
			}
		}
	})
}

func boundsOf(ps []metricdata.HistogramDataPoint[float64]) []float64 {
	if len(ps) == 0 {
		return nil
	}
	return ps[0].Bounds
}

// TestChunkTimings covers the first chunk and the gaps after it. A reasoning
// event is a chunk; a usage-only event is not; the time the caller takes to
// hand a chunk on is not upstream time.
func TestChunkTimings(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := observe(t, steps{
			{after: 300 * time.Millisecond, event: Event{Progress: true}},
			{after: 100 * time.Millisecond, event: Event{Delta: "a"}},
			{after: 100 * time.Millisecond, event: Event{Delta: "b", FinishReason: "stop"}},
			{after: 50 * time.Millisecond, event: Event{Usage: &Usage{PromptTokens: 1, CompletionTokens: 2}}},
		})
		slowCaller := func(Event) error { time.Sleep(time.Second); return nil }
		time.Sleep(time.Minute) // one decorator serves many calls, so each call starts its own clock
		if err := r.g.Stream(t.Context(), chatReq, slowCaller); err != nil {
			t.Fatal(err)
		}
		ms := r.metrics(t)
		first := histogram(t, ms, "gen_ai.client.inference.time_to_first_chunk")
		if len(first) != 1 || first[0].Count != 1 || first[0].Sum != 0.3 {
			t.Errorf("time to first chunk = %+v, want one record of 0.3 s, at the reasoning chunk", first)
		}
		per := histogram(t, ms, "gen_ai.client.inference.time_per_output_chunk")
		if len(per) != 1 || per[0].Count != 2 || per[0].Sum != 0.2 {
			t.Errorf("time per output chunk = %+v, want 2 records totalling 0.2 s (no usage-only event, no hand-on time)", per)
		}
		if got := attrMap(r.only(t, spanName).Attributes())["gen_ai.response.time_to_first_chunk"]; got != "0.3" {
			t.Errorf("span time_to_first_chunk = %q, want 0.3", got)
		}
	})
}

func TestChatHasNoChunkTimings(t *testing.T) {
	t.Parallel()
	r := observe(t, steps{})
	if _, err := r.g.Chat(t.Context(), chatReq); err != nil {
		t.Fatal(err)
	}
	a := attrMap(r.only(t, spanName).Attributes())
	if _, ok := a["gen_ai.request.stream"]; ok || a["gen_ai.response.model"] != "served-model" || a["gen_ai.usage.output_tokens"] != "2" {
		t.Errorf("span attributes = %v, want no stream flag, the response model and the usage", a)
	}
	ms := r.metrics(t)
	if len(histogram(t, ms, "gen_ai.client.inference.duration")) != 1 ||
		histogram(t, ms, "gen_ai.client.inference.time_to_first_chunk") != nil {
		t.Errorf("a non-streamed call wants a duration and no chunk timings")
	}
}

func TestErrorsAreClassifiedByKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		err      error
		afterOne bool // fail after one chunk, as a cut stream does
		want     string
	}{
		{"rate limited", &Error{Provider: "groq", Kind: KindRateLimited}, false, "rate_limited"},
		{"auth", &Error{Provider: "groq", Kind: KindAuth}, false, "auth"},
		{"bad request", &Error{Provider: "groq", Kind: KindBadRequest}, false, "bad_request"},
		{"timeout", &Error{Provider: "groq", Kind: KindTimeout}, false, "timeout"},
		{"cut stream", &Error{Provider: "groq", StatusCode: 200, Kind: KindUpstreamUnavailable}, true, "upstream_unavailable"},
		{"not a provider error", errors.New("encode request"), false, "_OTHER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			script := steps{{after: time.Millisecond, err: tc.err}}
			if tc.afterOne {
				script = slices.Insert(script, 0, step{after: time.Millisecond, event: Event{Delta: "partial"}})
			}
			r := observe(t, script)
			if err := r.g.Stream(t.Context(), chatReq, drain); !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want the inner error unchanged", err)
			}
			s := r.only(t, spanName)
			a := attrMap(s.Attributes())
			if a["error.type"] != tc.want || s.Status().Code != codes.Error || a["gen_ai.response.finish_reasons"] != `["error"]` {
				t.Errorf("error.type %q, status %v, finish_reasons %s; want %q, Error, [\"error\"]",
					a["error.type"], s.Status().Code, a["gen_ai.response.finish_reasons"], tc.want)
			}
			if strings.Contains(s.Status().Description, "encode") {
				t.Errorf("span status carries the error text %q", s.Status().Description)
			}
			d := histogram(t, r.metrics(t), "gen_ai.client.inference.duration")
			if len(d) != 1 || attrMap(d[0].Attributes.ToSlice())["error.type"] != tc.want {
				t.Errorf("duration points = %+v, want one with error.type %q", d, tc.want)
			}
		})
	}

	t.Run("a cancel is not a fault", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := observe(t, steps{{after: time.Millisecond, event: Event{Delta: "partial"}}, {after: time.Hour, event: Event{Delta: "never"}}})
			ctx, cancel := context.WithCancel(t.Context())
			err := r.g.Stream(ctx, chatReq, func(Event) error { cancel(); return nil })
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled: the caller's cancel must reach the upstream", err)
			}
			s := r.only(t, spanName)
			a := attrMap(s.Attributes())
			if a["error.type"] != "cancelled" || s.Status().Code != codes.Unset || a["gen_ai.response.finish_reasons"] != `["error"]` {
				t.Errorf("error.type %q, status %v, finish_reasons %s; want cancelled, Unset, [\"error\"]",
					a["error.type"], s.Status().Code, a["gen_ai.response.finish_reasons"])
			}
		})
	})
}

func TestWatchdogTimeoutsAreCountedByDeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		deadline string
		script   steps
	}{
		{"first_token", steps{{after: time.Second, event: Event{Delta: "late"}}}},
		{"stall", steps{{after: 10 * time.Millisecond, event: Event{Delta: "a"}}, {after: time.Second, event: Event{Delta: "late"}}}},
	} {
		t.Run(tc.deadline, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				r := observe(t, &Watchdog{Name: "groq", Inner: tc.script, FirstEvent: 100 * time.Millisecond, Stall: 100 * time.Millisecond})
				_ = r.g.Stream(t.Context(), chatReq, drain)
				ps := counter(r.metrics(t), "llm_gateway.provider.watchdog.timeouts")
				if len(ps) != 1 || ps[0].Value != 1 {
					t.Fatalf("timeouts = %+v, want one", ps)
				}
				a := attrMap(ps[0].Attributes.ToSlice())
				if a["llm_gateway.deadline"] != tc.deadline || a["gen_ai.provider.name"] != "groq" {
					t.Errorf("timeout attributes = %v, want deadline %s for groq", a, tc.deadline)
				}
				if got := attrMap(r.only(t, spanName).Attributes())["error.type"]; got != "timeout" {
					t.Errorf("error.type = %q, want timeout", got)
				}
			})
		})
	}
}

func TestUsageIsCounted(t *testing.T) {
	t.Parallel()
	tokens := func(r recorded, t *testing.T) map[string]int64 {
		out := map[string]int64{}
		ms := r.metrics(t)
		for _, dir := range []string{"input", "output"} {
			for _, p := range counter(ms, "gen_ai.client.inference.usage."+dir+"_tokens") {
				out[dir+" "+attrMap(p.Attributes.ToSlice())["gen_ai.token.modality"]] += p.Value
			}
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		script steps
		want   map[string]int64
	}{
		{"reported", steps{{event: Event{Delta: "a", Usage: &Usage{PromptTokens: 12, CompletionTokens: 34}}}},
			map[string]int64{"input text": 12, "output text": 34}},
		{"a failure with no usage counts 0 as unknown", steps{{err: &Error{Provider: "groq", Kind: KindUpstreamUnavailable}}},
			map[string]int64{"input unknown": 0, "output unknown": 0}},
		{"an unknown prompt count is left out", steps{{event: Event{Delta: "a", Usage: &Usage{CompletionTokens: 34, PromptTokensUnknown: true}}}},
			map[string]int64{"output text": 34}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := observe(t, tc.script)
			_ = r.g.Stream(t.Context(), chatReq, drain)
			got := tokens(r, t)
			if len(got) != len(tc.want) {
				t.Errorf("token points = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if n, ok := got[k]; !ok || n != v {
					t.Errorf("tokens %s = %d (recorded %v), want %d", k, n, ok, v)
				}
			}
		})
	}
}

func TestServerAttributesComeFromTheBaseURL(t *testing.T) {
	t.Parallel()
	for url, want := range map[string]string{
		"https://api.groq.com/openai/v1": "api.groq.com:443",
		"http://ollama:11434":            "ollama:11434",
		"http://fake":                    "fake:80",
	} {
		a := attrMap(serverAttrs(url))
		if got := a["server.address"] + ":" + a["server.port"]; got != want {
			t.Errorf("serverAttrs(%q) = %s, want %s", url, got, want)
		}
	}
}

func TestProviderTimingsAreSpanAttributesOnly(t *testing.T) {
	t.Parallel()
	d := func(n time.Duration) *time.Duration { return &n }
	keys := []string{
		"llm_gateway.provider.queue_duration", "llm_gateway.provider.load_duration", "llm_gateway.provider.prefill_duration",
		"llm_gateway.provider.decode_duration", "llm_gateway.provider.total_duration",
	}
	for _, tc := range []struct {
		name    string
		timings *Timings
		want    map[string]time.Duration // a key not here must be absent
	}{
		{"groq reports no load", &Timings{Queue: d(83291930), Prefill: d(1039105), Decode: d(7396903), Total: d(8436008)},
			map[string]time.Duration{keys[0]: 83291930, keys[2]: 1039105, keys[3]: 7396903, keys[4]: 8436008}},
		{"ollama reports no queue, and a reported 0 is kept", &Timings{Load: d(0), Prefill: d(131822000), Decode: d(116869000), Total: d(346482167)},
			map[string]time.Duration{keys[1]: 0, keys[2]: 131822000, keys[3]: 116869000, keys[4]: 346482167}},
		{"openai reports none", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := observe(t, steps{{event: Event{Delta: "a", Usage: &Usage{PromptTokens: 1, CompletionTokens: 1, Timings: tc.timings}}}})
			if err := r.g.Stream(t.Context(), chatReq, drain); err != nil {
				t.Fatal(err)
			}
			got := map[string]float64{}
			for _, kv := range r.only(t, spanName).Attributes() {
				if strings.HasPrefix(string(kv.Key), "llm_gateway.provider.") {
					got[string(kv.Key)] = kv.Value.AsFloat64()
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("timing attributes = %v, want %v", got, tc.want)
			}
			for k, w := range tc.want {
				if v, ok := got[k]; !ok || v != w.Seconds() {
					t.Errorf("%s = %v (set %v), want %v seconds", k, v, ok, w.Seconds())
				}
			}
			for name, m := range r.metrics(t) {
				for _, set := range pointAttrs(m) {
					for _, k := range keys {
						if set.HasValue(attribute.Key(k)) {
							t.Errorf("%s carries %s; a timing goes on the span only", name, k)
						}
					}
				}
			}
		})
	}
}
