package httpapi_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
)

func metered(t *testing.T) (*sdkmetric.ManualReader, *sdkmetric.MeterProvider) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return reader, mp
}

func collectMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) (metricdata.Metrics, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

// countsBy sums a counter by the values of the given attributes, joined with spaces.
func countsBy(t *testing.T, reader *sdkmetric.ManualReader, name string, keys ...string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	m, ok := collectMetric(t, reader, name)
	if !ok {
		return out
	}
	for _, p := range m.Data.(metricdata.Sum[int64]).DataPoints {
		var vals []string
		for _, k := range keys {
			v, _ := p.Attributes.Value(attribute.Key(k))
			vals = append(vals, v.AsString())
		}
		if p.Attributes.Len() != len(keys) {
			t.Errorf("%s point has attributes %v, want only %v", name, p.Attributes.ToSlice(), keys)
		}
		out[strings.Join(vals, " ")] += p.Value
	}
	return out
}

func equalCounts(got, want map[string]int64) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// finish reads the whole response, then waits for the request's log line,
// which is written after its metrics.
func finish(t *testing.T, g gateway, resp *http.Response) {
	t.Helper()
	_, _ = io.Copy(io.Discard, resp.Body)
	logLine(t, g)
}

func TestStreamCommitsAreCountedByReason(t *testing.T) {
	t.Parallel()
	reader, mp := metered(t)
	for model, stream := range map[string]bool{
		"fake?tokens=3": true, "fake?first_token=400ms&tokens=2": true, "fake?status=502": true,
		"fake?tokens=4": false, // a non-streamed answer sends its status at the end, with no hold
	} {
		g := start(t, setup{upstreamModel: model, hold: 100 * time.Millisecond, meter: mp})
		resp := post(t.Context(), t, g, body(stream, ""))
		finish(t, g, resp)
		_ = resp.Body.Close()
	}
	got := countsBy(t, reader, "llm_gateway.chat.stream.commits", "llm_gateway.commit.reason")
	if want := map[string]int64{"first_token": 1, "hold_elapsed": 1, "error": 1}; !equalCounts(got, want) {
		t.Errorf("commits = %v, want %v", got, want)
	}
}

func TestATruncatedStreamIsCountedByErrorKindNotStatus(t *testing.T) {
	t.Parallel()
	reader, mp := metered(t)
	for model, status := range map[string]int{
		"fake?tokens=5&drop_after=2":      http.StatusOK,
		"fake?tokens=5&malformed_after=2": http.StatusOK,
		"fake?tokens=5&stall_after=2":     http.StatusOK,
		"fake?status=502":                 http.StatusBadGateway, // failed before the commit: a real status, not a cut stream
	} {
		g := start(t, setup{upstreamModel: model, stall: 100 * time.Millisecond, meter: mp})
		resp := post(t.Context(), t, g, body(true, ""))
		if resp.StatusCode != status {
			t.Errorf("%s: status = %d, want %d", model, resp.StatusCode, status)
		}
		finish(t, g, resp)
		_ = resp.Body.Close()
	}
	got := countsBy(t, reader, "llm_gateway.chat.stream.truncations", "gen_ai.provider.name", "error.type")
	if want := map[string]int64{"fake upstream_unavailable": 2, "fake timeout": 1}; !equalCounts(got, want) {
		t.Errorf("truncations = %v, want %v", got, want)
	}
}

func TestClientDisconnectsAreCountedByStage(t *testing.T) {
	t.Parallel()
	reader, mp := metered(t)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	leave := func(g gateway, afterFirstToken bool) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url+"/v1/chat/completions", strings.NewReader(body(true, "")))
		if err != nil {
			t.Fatal(err)
		}
		if !afterFirstToken {
			time.AfterFunc(100*time.Millisecond, cancel) // during the hold: no status sent yet
			if resp, err := client.Do(req); err == nil {
				_ = resp.Body.Close()
				t.Errorf("got status %d during the hold, want none", resp.StatusCode)
			}
		} else {
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "data: ") {
					break // the first token arrived; now leave mid-stream
				}
			}
			cancel()
			_ = resp.Body.Close()
		}
		logLine(t, g)
	}
	leave(start(t, setup{upstreamModel: "fake?first_token=3s", hold: 5 * time.Second, meter: mp}), false)
	leave(start(t, setup{upstreamModel: "fake?tokens=100&tpot=50ms", meter: mp}), true)

	got := countsBy(t, reader, "llm_gateway.chat.client_disconnects", "llm_gateway.stage")
	if want := map[string]int64{"before_commit": 1, "after_commit": 1}; !equalCounts(got, want) {
		t.Errorf("disconnects = %v, want %v", got, want)
	}
}

// delayed is an upstream whose first token takes the given time.
type delayed time.Duration

func (d delayed) Chat(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (d delayed) Stream(_ context.Context, _ provider.Request, fn func(provider.Event) error) error {
	time.Sleep(time.Duration(d))
	if err := fn(provider.Event{Delta: "hi"}); err != nil {
		return err
	}
	return fn(provider.Event{FinishReason: "stop"})
}

// slowBody delivers the request body only after a delay, as a slow client does.
type slowBody struct {
	delay time.Duration
	r     io.Reader
}

func (b *slowBody) Read(p []byte) (int, error) {
	time.Sleep(b.delay)
	b.delay = 0
	return b.r.Read(p)
}

func TestTheGatewaysTimeToFirstTokenStartsAtArrival(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		reader, mp := metered(t)
		h := httpapi.New(slog.New(slog.DiscardHandler), httpapi.Deps{
			Routes:    resolver{"chat": {{ProviderName: "groq", Provider: delayed(100 * time.Millisecond), UpstreamModel: "m"}}},
			Chat:      httpapi.ChatConfig{StatusHold: time.Second, HeartbeatInterval: time.Second, MaxBodyBytes: 4 << 10},
			Telemetry: httpapi.Telemetry{Meter: mp},
		})
		for _, stream := range []bool{true, false} {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
				&slowBody{delay: 300 * time.Millisecond, r: strings.NewReader(body(stream, ""))})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("stream %v: status = %d, want 200", stream, w.Code)
			}
		}

		m, ok := collectMetric(t, reader, "llm_gateway.chat.time_to_first_token")
		if !ok {
			t.Fatal("no llm_gateway.chat.time_to_first_token recorded")
		}
		ps := m.Data.(metricdata.Histogram[float64]).DataPoints
		// 0.3 s reading the body, then 0.1 s to the upstream's first token.
		if m.Unit != "s" || len(ps) != 1 || ps[0].Count != 1 || ps[0].Sum != 0.4 {
			t.Fatalf("time to first token = %+v (unit %q), want one record of 0.4 s for the stream only", ps, m.Unit)
		}
		if v, _ := ps[0].Attributes.Value(attribute.Key("gen_ai.provider.name")); ps[0].Attributes.Len() != 1 || v.AsString() != "groq" {
			t.Errorf("attributes = %v, want only gen_ai.provider.name=groq", ps[0].Attributes.ToSlice())
		}
	})
}
