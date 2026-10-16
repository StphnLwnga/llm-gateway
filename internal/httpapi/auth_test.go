package httpapi

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/StphnLwnga/llm-gateway/internal/authcache"
	"github.com/StphnLwnga/llm-gateway/internal/telemetry"
	"github.com/StphnLwnga/llm-gateway/internal/tenant"
)

// countingKeys counts lookups and fails each one with err, or uses FakeKeys when err is nil.
type countingKeys struct {
	calls atomic.Int64
	err   error
	ctx   func(context.Context)
}

func (k *countingKeys) Lookup(ctx context.Context, hash []byte) (tenant.Identity, error) {
	k.calls.Add(1)
	if k.ctx != nil {
		k.ctx(ctx)
	}
	if k.err != nil {
		return tenant.Identity{}, k.err
	}
	return FakeKeys{}.Lookup(ctx, hash)
}

// touchingKeys is FakeKeys with the Touch the auth cache needs.
type touchingKeys struct{ FakeKeys }

func (touchingKeys) Touch(context.Context, []string) error { return nil }

func request(t *testing.T, h http.Handler, method, path, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestEveryKeyFailureGetsTheSame401(t *testing.T) {
	t.Parallel()
	unknown, err := tenant.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	last := FakeKey[len(FakeKey)-1]
	flipped := FakeKey[:len(FakeKey)-1] + map[bool]string{true: "1", false: "0"}[last == '0']

	cases := []struct {
		name, authorization, reason string
		reachesLookup               bool
	}{
		{"missing", "", "missing", false},
		{"basic scheme", "Basic " + FakeKey, "not_bearer", false},
		{"bearer and nothing", "Bearer", "not_bearer", false},
		{"malformed", "Bearer gw_short", "malformed", false},
		{"bad checksum", "Bearer " + flipped, "bad_checksum", false},
		{"unknown, revoked or disabled", "Bearer " + unknown, "no_active_key", true},
	}
	var bodies []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			keys := &countingKeys{}
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
			h := New(slog.New(slog.NewTextHandler(&logs, nil)), Deps{Keys: keys, Telemetry: Telemetry{Meter: mp}})
			w := request(t, h, http.MethodGet, "/v1/models", tc.authorization)
			if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("status %d, WWW-Authenticate %q; want 401 and Bearer", w.Code, w.Header().Get("WWW-Authenticate"))
			}
			bodies = append(bodies, w.Body.String())
			if got := keys.calls.Load() > 0; got != tc.reachesLookup {
				t.Errorf("reached the key lookup = %v, want %v", got, tc.reachesLookup)
			}
			if !strings.Contains(logs.String(), "reason="+tc.reason) {
				t.Errorf("log does not record reason %s:\n%s", tc.reason, logs.String())
			}
			if got := authFailures(t, reader); len(got) != 1 || got[tc.reason] != 1 {
				t.Errorf("llm_gateway.auth.failures = %v, want 1 with reason %s", got, tc.reason)
			}
		})
	}
	want := `{"error":{"message":"` + invalidKeyMessage + `","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}` + "\n"
	for i, b := range bodies {
		if b != want {
			t.Errorf("%s: body =\n%s\nwant\n%s", cases[i].name, b, want)
		}
	}
}

func TestEveryV1PathNeedsAKeyButProbesDoNot(t *testing.T) {
	t.Parallel()
	h := New(discard, Deps{Keys: FakeKeys{}})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/models"},
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodDelete, "/v1/chat/completions"},
		{http.MethodGet, "/v1/nope"},
	} {
		if w := request(t, h, tc.method, tc.path, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no key = %d, want 401", tc.method, tc.path, w.Code)
		}
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if w := request(t, h, http.MethodGet, path, ""); w.Code != http.StatusOK {
			t.Errorf("GET %s with no key = %d, want 200", path, w.Code)
		}
	}
	if w := request(t, h, http.MethodGet, "/v1/models", "bearer "+FakeKey); w.Code != http.StatusOK {
		t.Errorf("GET /v1/models with a lowercase scheme = %d, want 200: the scheme is case-insensitive", w.Code)
	}
}

func TestAFailedLookupIsAServerErrorNotA401(t *testing.T) {
	t.Parallel()
	type marker struct{}
	var sawMarker atomic.Bool
	keys := &countingKeys{err: errors.New("dial tcp: connection refused"), ctx: func(ctx context.Context) {
		sawMarker.Store(ctx.Value(marker{}) != nil)
	}}
	var logs strings.Builder
	h := New(slog.New(slog.NewTextHandler(&logs, nil)), Deps{Keys: keys})
	r := httptest.NewRequestWithContext(context.WithValue(t.Context(), marker{}, true), http.MethodGet, "/v1/models", nil)
	WithKey(r)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"type":"server_error"`) {
		t.Errorf("GET /v1/models with the key store down = %d %s, want a 500 server_error", w.Code, w.Body.String())
	}
	if !sawMarker.Load() {
		t.Error("the lookup did not get the request's context")
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("the lookup failure is not logged:\n%s", logs.String())
	}
}

func TestAuthIsTracedAndCountedAndTheKeyNeverLeaks(t *testing.T) {
	t.Parallel()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})
	var logs strings.Builder
	log := telemetry.NewLogger(&logs, slog.LevelDebug, "json")
	cache, err := authcache.New(touchingKeys{}, time.Minute, log, mp)
	if err != nil {
		t.Fatal(err)
	}
	h := New(log, Deps{
		Routes:    routes{"down": {{ProviderName: "down", Provider: unavailable{}}}},
		Keys:      cache,
		Telemetry: Telemetry{Tracer: tp, Meter: mp, ServerAddr: "llm-gateway:8080"},
	})
	unknown, err := tenant.NewKey()
	if err != nil {
		t.Fatal(err)
	}

	request(t, h, http.MethodGet, "/v1/models", "Bearer "+unknown)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, WithKey(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"down","messages":[{"role":"user","content":"hi"}]}`))))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("chat with the upstream down = %d, want 502", w.Code)
	}
	if hit := request(t, h, http.MethodGet, "/v1/models", "Bearer "+FakeKey); hit.Code != http.StatusOK {
		t.Fatalf("GET /v1/models with the key cached = %d, want 200", hit.Code)
	}

	ended := spans.Ended()
	if len(ended) != 3 {
		t.Fatalf("recorded %d spans, want 3", len(ended))
	}
	rejected, served := attrsOf(ended[0].Attributes()), attrsOf(ended[1].Attributes())
	if rejected["http.response.status_code"] != "401" || rejected["http.route"] != "/v1/models" {
		t.Errorf("401 span: status %q, route %q; want 401 and /v1/models", rejected["http.response.status_code"], rejected["http.route"])
	}
	if _, ok := rejected[tenant.SpanAttribute]; ok {
		t.Error("a rejected request's span names a tenant")
	}
	if served[tenant.SpanAttribute] != FakeIdentity.TenantID {
		t.Errorf("server span %s = %q, want %s", tenant.SpanAttribute, served[tenant.SpanAttribute], FakeIdentity.TenantID)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	var counted401 uint64
	seen := map[string]bool{}
	hash := tenant.HashKey(FakeKey)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen[m.Name] = true
			for _, set := range metricAttrSets(m) {
				for k, v := range attrsOf(set) {
					if strings.Contains(k, "tenant") || v == FakeIdentity.TenantID || v == FakeIdentity.KeyID ||
						v == string(hash) || v == hex.EncodeToString(hash) {
						t.Errorf("metric %s carries %s = %q; a tenant, key ID or key hash on a metric is a new series per caller", m.Name, k, v)
					}
				}
			}
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == "http.server.request.duration" {
				for _, p := range h.DataPoints {
					if attrsOf(p.Attributes.ToSlice())["http.response.status_code"] == "401" {
						counted401 += p.Count
					}
				}
			}
		}
	}
	if counted401 != 1 {
		t.Errorf("http.server.request.duration counted %d requests with status 401, want 1", counted401)
	}
	for _, name := range []string{"llm_gateway.auth.failures", "llm_gateway.auth.cache.lookups", "llm_gateway.auth.cache.size", "llm_gateway.auth.lookup.duration"} {
		if !seen[name] {
			t.Errorf("no %s recorded, so the check above did not cover it", name)
		}
	}
	if got := authFailures(t, reader); len(got) != 1 || got["no_active_key"] != 1 {
		t.Errorf("llm_gateway.auth.failures = %v, want 1 with reason no_active_key and none for the accepted key", got)
	}

	var recorded strings.Builder
	for _, s := range ended {
		recorded.WriteString(s.Name())
		for _, kv := range s.Attributes() {
			recorded.WriteString(string(kv.Key) + "=" + kv.Value.String() + "\n")
		}
		for _, e := range s.Events() {
			for _, kv := range e.Attributes {
				recorded.WriteString(string(kv.Key) + "=" + kv.Value.String() + "\n")
			}
		}
	}
	recorded.WriteString(logs.String())
	recorded.WriteString(w.Body.String())
	for _, secret := range []string{FakeKey, unknown, FakeKey[3:20], unknown[3:20], "Bearer"} {
		if strings.Contains(recorded.String(), secret) {
			t.Errorf("a span, log line or response holds %q:\n%s", secret, recorded.String())
		}
	}
}

func TestRequestLogLinesCarryTheTenantAndKey(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	h := New(telemetry.NewLogger(&logs, slog.LevelInfo, "text"), Deps{
		Routes: routes{"down": {{ProviderName: "down", Provider: unavailable{}}}},
		Keys:   FakeKeys{},
	})
	h.ServeHTTP(httptest.NewRecorder(), WithKey(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"down","messages":[{"role":"user","content":"hi"}]}`))))
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("want the upstream error and the chat completion lines, got:\n%s", logs.String())
	}
	for _, l := range lines {
		if !strings.Contains(l, "tenant_id="+FakeIdentity.TenantID) || !strings.Contains(l, "key_id="+FakeIdentity.KeyID) {
			t.Errorf("log line has no tenant_id and key_id: %s", l)
		}
	}
}

// authFailures returns llm_gateway.auth.failures by reason. A point with
// any other attribute is recorded under its attributes, so the caller sees it.
func authFailures(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "llm_gateway.auth.failures" {
				continue
			}
			for _, p := range m.Data.(metricdata.Sum[int64]).DataPoints {
				attrs := attrsOf(p.Attributes.ToSlice())
				if reason, ok := attrs["llm_gateway.auth.reason"]; ok && len(attrs) == 1 {
					out[reason] += p.Value
				} else {
					out[p.Attributes.Encoded(attribute.DefaultEncoder())] += p.Value
				}
			}
		}
	}
	return out
}

func metricAttrSets(m metricdata.Metrics) [][]attribute.KeyValue {
	var out [][]attribute.KeyValue
	switch d := m.Data.(type) {
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes.ToSlice())
		}
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes.ToSlice())
		}
	case metricdata.Gauge[int64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes.ToSlice())
		}
	}
	return out
}
