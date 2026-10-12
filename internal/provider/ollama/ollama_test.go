package ollama_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/provider/ollama"
	"github.com/StphnLwnga/llm-gateway/internal/provider/providertest"
)

// wire writes Ollama's NDJSON chat format for the contract suite.
type wire struct{}

func line(v map[string]any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

func msg(content string) map[string]string {
	return map[string]string{"role": "assistant", "content": content}
}

func (wire) Delta(text string) []byte {
	return line(map[string]any{"model": "m", "created_at": "2026-01-01T00:00:00Z", "message": msg(text), "done": false})
}

func final(content string, u provider.Usage, finish string) map[string]any {
	return map[string]any{"model": "m", "created_at": "2026-01-01T00:00:00Z", "message": msg(content), "done": true,
		"done_reason": finish, "total_duration": 900, "load_duration": 100, "prompt_eval_count": u.PromptTokens,
		"prompt_eval_duration": 200, "eval_count": u.CompletionTokens, "eval_duration": 600}
}

func (wire) End(u provider.Usage, finish string) []byte { return line(final("", u, finish)) }

func (wire) Chat(content string, u provider.Usage, finish string) []byte {
	return line(final(content, u, finish))
}

func (wire) Error(status int) []byte {
	return []byte(fmt.Sprintf(`{"error":%q}`, http.StatusText(status)))
}

func (wire) Malformed() []byte { return []byte("{\"message\":\n") }

func TestAdapterKeepsTheContract(t *testing.T) {
	providertest.Run(t, providertest.Adapter{
		Name: "ollama",
		New: func(_ *testing.T, baseURL string, client *http.Client) provider.Provider {
			return ollama.New("ollama", baseURL, client)
		},
		Wire: wire{},
	})
}

func serve(t *testing.T, status int, body []byte) (*ollama.Adapter, *[]byte, *string) {
	t.Helper()
	var got []byte
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return ollama.New("ollama", srv.URL, srv.Client()), &got, &path
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(os.DirFS("testdata"), name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func collect(t *testing.T, p provider.Provider) ([]provider.Event, error) {
	t.Helper()
	var evs []provider.Event
	err := p.Stream(t.Context(), provider.Request{Model: "m"}, func(e provider.Event) error {
		evs = append(evs, e)
		return nil
	})
	return evs, err
}

func text(evs []provider.Event) string {
	var b strings.Builder
	for _, e := range evs {
		b.WriteString(e.Delta)
	}
	return b.String()
}

func last(t *testing.T, evs []provider.Event) provider.Event {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	return evs[len(evs)-1]
}

func dur(d *time.Duration) string {
	if d == nil {
		return "unset"
	}
	return d.String()
}

func TestStreamRequestBodyAndPath(t *testing.T) {
	t.Parallel()
	a, got, path := serve(t, 200, wire{}.End(provider.Usage{PromptTokens: 1}, "stop"))
	temp, maxTokens, seed := 0.0, 64, int64(7)
	err := a.Stream(t.Context(), provider.Request{
		Model:       "llama3.2:3b",
		Messages:    []provider.Message{{Role: "user", Content: "hi"}},
		Temperature: &temp, MaxTokens: &maxTokens, Stop: []string{"\n"}, Seed: &seed,
	}, func(provider.Event) error { return nil })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if *path != "/api/chat" {
		t.Errorf("path = %s, want /api/chat", *path)
	}
	want := `{"model":"llama3.2:3b","messages":[{"role":"user","content":"hi"}],"stream":true,` +
		`"options":{"temperature":0,"num_predict":64,"stop":["\n"],"seed":7}}`
	if string(*got) != want {
		t.Errorf("request body:\n%s\nwant:\n%s", *got, want)
	}
}

func TestChatRequestBodyLeavesOutEmptyOptions(t *testing.T) {
	t.Parallel()
	a, got, _ := serve(t, 200, wire{}.Chat("ok", provider.Usage{PromptTokens: 1}, "stop"))
	if _, err := a.Chat(t.Context(), provider.Request{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if want := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`; string(*got) != want {
		t.Errorf("request body = %s, want %s", *got, want)
	}
}

func TestFinalLineCarriesUsageAndNanosecondTimings(t *testing.T) {
	t.Parallel()
	body := `{"message":{"role":"assistant","content":"Hi"},"done":false}` + "\n" +
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"length","total_duration":4883583458,` +
		`"load_duration":1334875,"prompt_eval_count":26,"prompt_eval_duration":342546000,"eval_count":282,"eval_duration":4535599000}` + "\n"
	a, _, _ := serve(t, 200, []byte(body))
	evs, err := collect(t, a)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ev := last(t, evs)
	if ev.FinishReason != "length" || ev.Usage == nil {
		t.Fatalf("last event = %+v, want finish length with usage", ev)
	}
	u := *ev.Usage
	if u.PromptTokens != 26 || u.CompletionTokens != 282 || u.TotalTokens != 308 || u.PromptTokensUnknown {
		t.Errorf("usage = %+v, want 26 + 282 = 308", u)
	}
	if u.Timings == nil {
		t.Fatal("no timings")
	}
	got := fmt.Sprintf("queue=%s load=%s prefill=%s decode=%s total=%s",
		dur(u.Timings.Queue), dur(u.Timings.Load), dur(u.Timings.Prefill), dur(u.Timings.Decode), dur(u.Timings.Total))
	if want := "queue=unset load=1.334875ms prefill=342.546ms decode=4.535599s total=4.883583458s"; got != want {
		t.Errorf("timings = %s, want %s", got, want)
	}
}

func TestMissingPromptEvalCountIsUnknownNotZero(t *testing.T) {
	t.Parallel()
	body := `{"message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","eval_count":3,"eval_duration":1000}` + "\n"
	a, _, _ := serve(t, 200, []byte(body))
	evs, err := collect(t, a)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	u := last(t, evs).Usage
	if u == nil || !u.PromptTokensUnknown || u.PromptTokens != 0 || u.TotalTokens != 0 || u.CompletionTokens != 3 {
		t.Errorf("usage = %+v, want prompt unknown, total 0, completion 3", u)
	}
	if u != nil && u.Timings != nil && (u.Timings.Load != nil || u.Timings.Prefill != nil || u.Timings.Total != nil) {
		t.Errorf("timings = %+v, want load, prefill and total unset", u.Timings)
	}
}

func TestDoneReasons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		reason string
		finish string
		kind   provider.Kind // empty when there is no error
	}{
		{"stop", "stop", ""},
		{"length", "length", ""},
		{"load", "", provider.KindBadRequest},
		{"unload", "", provider.KindBadRequest},
		{"", "", provider.KindUpstreamUnavailable},
		{"something_new", "", provider.KindUpstreamUnavailable},
	}
	for _, tc := range cases {
		t.Run("done_reason="+tc.reason, func(t *testing.T) {
			t.Parallel()
			body := fmt.Sprintf(`{"message":{"role":"assistant","content":""},"done":true,"done_reason":%q,"prompt_eval_count":1}`+"\n", tc.reason)
			a, _, _ := serve(t, 200, []byte(body))
			evs, err := collect(t, a)
			if tc.kind == "" {
				if err != nil || last(t, evs).FinishReason != tc.finish {
					t.Errorf("err = %v, events = %+v; want finish %s", err, evs, tc.finish)
				}
				return
			}
			var pe *provider.Error
			if !errors.As(err, &pe) || pe.Kind != tc.kind {
				t.Errorf("err = %v, want kind %s", err, tc.kind)
			}
		})
	}
}

func TestErrorObjects(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ body, message string }{
		"at the start": {`{"error":"model is loading"}` + "\n", "model is loading"},
		"mid-stream":   {string(wire{}.Delta("a")) + `{"error":"out of memory"}` + "\n" + string(wire{}.Delta("b")), "out of memory"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a, _, _ := serve(t, 200, []byte(tc.body))
			evs, err := collect(t, a)
			var pe *provider.Error
			if !errors.As(err, &pe) || pe.Kind != provider.KindUpstreamUnavailable || pe.StatusCode != 200 {
				t.Errorf("err = %v, want upstream_unavailable with status 200", err)
			}
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Errorf("err = %v, want it to carry the upstream message %q", err, tc.message)
			}
			if strings.Contains(text(evs), "b") {
				t.Errorf("content after the error object was forwarded: %q", text(evs))
			}
		})
	}
}

func TestUnknownModelIs404BadRequest(t *testing.T) {
	t.Parallel()
	a, _, _ := serve(t, 404, []byte(`{"error":"model \"nope\" not found, try pulling it first"}`))
	_, err := a.Chat(t.Context(), provider.Request{Model: "nope"})
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.StatusCode != 404 || pe.Kind != provider.KindBadRequest {
		t.Errorf("err = %v, want status 404, kind bad_request", err)
	}
}

func TestStreamWithoutDoneIsAnError(t *testing.T) {
	t.Parallel()
	a, _, _ := serve(t, 200, wire{}.Delta("cut off"))
	evs, err := collect(t, a)
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.KindUpstreamUnavailable {
		t.Fatalf("err = %v, want upstream_unavailable", err)
	}
	if text(evs) != "cut off" {
		t.Errorf("text = %q, want %q", text(evs), "cut off")
	}
}

func TestStreamReadsALineLargerThan64KiB(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 100<<10)
	a, _, _ := serve(t, 200, append(wire{}.Delta(big), wire{}.End(provider.Usage{PromptTokens: 1}, "stop")...))
	evs, err := collect(t, a)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if text(evs) != big {
		t.Errorf("got %d bytes, want %d", len(text(evs)), len(big))
	}
}

func TestThinkingIsNotForwarded(t *testing.T) {
	t.Parallel()
	// constructed from Ollama's API docs (message.thinking for thinking models), not recorded
	body := `{"message":{"role":"assistant","content":"","thinking":"Let me think."},"done":false}` + "\n" +
		`{"message":{"role":"assistant","content":"Blue."},"done":false}` + "\n" +
		string(wire{}.End(provider.Usage{PromptTokens: 5, CompletionTokens: 9}, "stop"))
	a, _, _ := serve(t, 200, []byte(body))
	evs, err := collect(t, a)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if text(evs) != "Blue." {
		t.Errorf("text = %q, want only the content %q", text(evs), "Blue.")
	}
	if len(evs) == 0 || !evs[0].Progress || evs[0].Delta != "" {
		t.Errorf("first event = %+v, want a progress event for the thinking line", evs)
	}
}

func TestRecordedFixtures(t *testing.T) {
	t.Parallel()
	// recorded from Ollama 0.40.3 (llama3.2:3b); see testdata/README.md
	wantTimings := map[string]string{
		"ollama_stream_recorded.ndjson": "load=51.284584ms prefill=131.822ms decode=116.869ms total=346.482167ms",
		"ollama_chat_recorded.json":     "load=2.609541ms prefill=22.913ms decode=111.514ms total=139.11275ms",
	}
	check := func(t *testing.T, file, content, finish, model string, u provider.Usage) {
		t.Helper()
		if model != "llama3.2:3b" {
			t.Errorf("model = %q, want llama3.2:3b", model)
		}
		if content != "Red, Blue, Yellow" || finish != "stop" {
			t.Errorf("content, finish = %q, %q; want %q, stop", content, finish, "Red, Blue, Yellow")
		}
		if u.PromptTokens != 33 || u.CompletionTokens != 6 || u.TotalTokens != 39 || u.PromptTokensUnknown {
			t.Errorf("usage = %+v, want 33 + 6 = 39", u)
		}
		if u.Timings == nil {
			t.Fatal("no timings")
		}
		got := fmt.Sprintf("load=%s prefill=%s decode=%s total=%s", dur(u.Timings.Load), dur(u.Timings.Prefill), dur(u.Timings.Decode), dur(u.Timings.Total))
		if got != wantTimings[file] {
			t.Errorf("timings = %s, want %s", got, wantTimings[file])
		}
	}

	t.Run("stream", func(t *testing.T) {
		t.Parallel()
		a, _, _ := serve(t, 200, fixture(t, "ollama_stream_recorded.ndjson"))
		evs, err := collect(t, a)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		ev := last(t, evs)
		if ev.Usage == nil {
			t.Fatal("no usage")
		}
		for _, e := range evs {
			if e.Model != "llama3.2:3b" {
				t.Errorf("event %+v names model %q, want llama3.2:3b on every event", e, e.Model)
			}
		}
		check(t, "ollama_stream_recorded.ndjson", text(evs), ev.FinishReason, ev.Model, *ev.Usage)
	})

	t.Run("chat", func(t *testing.T) {
		t.Parallel()
		a, _, _ := serve(t, 200, fixture(t, "ollama_chat_recorded.json"))
		resp, err := a.Chat(t.Context(), provider.Request{Model: "m"})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		check(t, "ollama_chat_recorded.json", resp.Content, resp.FinishReason, resp.Model, resp.Usage)
	})
}
