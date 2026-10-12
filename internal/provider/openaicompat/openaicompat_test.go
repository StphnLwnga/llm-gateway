package openaicompat_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/provider/openaicompat"
	"github.com/StphnLwnga/llm-gateway/internal/provider/providertest"
)

const testKey = config.Secret("sk-test-0123456789abcdef")

// wire writes the OpenAI chat completions wire format for the contract suite.
type wire struct{}

func chunk(v map[string]any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return []byte("data: " + string(b) + "\n\n")
}

func choice(delta map[string]any, finish any) map[string]any {
	return map[string]any{"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 0, "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}, "usage": nil}
}

func (wire) Delta(text string) []byte {
	return chunk(choice(map[string]any{"content": text}, nil))
}

func (wire) End(u provider.Usage, finish string) []byte {
	var b []byte
	b = append(b, chunk(choice(map[string]any{}, finish))...)
	b = append(b, chunk(map[string]any{"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 0, "model": "m",
		"choices": []any{}, "usage": map[string]int{"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens, "total_tokens": u.TotalTokens}})...)
	return append(b, "data: [DONE]\n\n"...)
}

func (wire) Chat(content string, u provider.Usage, finish string) []byte {
	b, err := json.Marshal(map[string]any{"id": "chatcmpl-test", "object": "chat.completion", "created": 0, "model": "m",
		"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": content}, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens, "total_tokens": u.TotalTokens}})
	if err != nil {
		panic(err)
	}
	return b
}

func (wire) Error(status int) []byte {
	return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"server_error","param":null,"code":null}}`, http.StatusText(status)))
}

func (wire) Malformed() []byte { return []byte("data: {\"id\":\n\n") }

func TestAdapterKeepsTheContract(t *testing.T) {
	providertest.Run(t, providertest.Adapter{
		Name: "openai",
		New: func(_ *testing.T, baseURL string, client *http.Client) provider.Provider {
			return openaicompat.New("openai", baseURL, testKey, client)
		},
		Wire: wire{},
	})
}

// serve returns an adapter whose upstream answers every request with body,
// and records the last request body.
func serve(t *testing.T, contentType string, body []byte) (*openaicompat.Adapter, *[]byte) {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return openaicompat.New("test", srv.URL, testKey, srv.Client()), &got
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

type summary struct {
	Text    string
	Finish  []string
	Usage   []provider.Usage // token counts only; timings are checked separately
	Ordered bool             // no delta after the finish reason or the usage
}

func summarize(evs []provider.Event) summary {
	s := summary{Ordered: true}
	var text strings.Builder
	ended := false
	for _, e := range evs {
		if e.Delta != "" && ended {
			s.Ordered = false
		}
		text.WriteString(e.Delta)
		if e.FinishReason != "" {
			s.Finish = append(s.Finish, e.FinishReason)
			ended = true
		}
		if e.Usage != nil {
			s.Usage = append(s.Usage, provider.Usage{PromptTokens: e.Usage.PromptTokens, CompletionTokens: e.Usage.CompletionTokens, TotalTokens: e.Usage.TotalTokens})
			ended = true
		}
	}
	s.Text = text.String()
	return s
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(os.DirFS("testdata"), name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStreamFixtures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		file string
		want summary
	}{
		{
			// constructed from the OpenAI API reference, not recorded
			"openai_stream_constructed.sse",
			summary{Text: "Hello! How can I assist you today?", Finish: []string{"stop"},
				Usage: []provider.Usage{{PromptTokens: 19, CompletionTokens: 10, TotalTokens: 29}}, Ordered: true},
		},
		{
			// recorded from Groq: usage both under x_groq on the finish chunk and in the include_usage chunk
			"groq_stream_recorded.sse",
			summary{Text: "الأحمر، والأزرق، والأصفر ", Finish: []string{"stop"},
				Usage: []provider.Usage{{PromptTokens: 16, CompletionTokens: 10, TotalTokens: 26}}, Ordered: true},
		},
		{
			// recorded from Groq (gpt-oss): every token is delta.reasoning, so no content reaches the caller
			"groq_reasoning_stream_recorded.sse",
			summary{Text: "", Finish: []string{"length"},
				Usage: []provider.Usage{{PromptTokens: 79, CompletionTokens: 32, TotalTokens: 111}}, Ordered: true},
		},
		{
			// constructed from the Groq API reference: usage only under x_groq, not recorded
			"groq_stream_xgroq_constructed.sse",
			summary{Text: "Low latency matters.", Finish: []string{"stop"},
				Usage: []provider.Usage{{PromptTokens: 18, CompletionTokens: 4, TotalTokens: 22}}, Ordered: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			a, _ := serve(t, "text/event-stream", fixture(t, tc.file))
			evs, err := collect(t, a)
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if got := summarize(evs); !equalSummary(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func equalSummary(a, b summary) bool {
	return a.Text == b.Text && slices.Equal(a.Finish, b.Finish) && slices.Equal(a.Usage, b.Usage) && a.Ordered == b.Ordered
}

func TestChatFixtures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		file string
		want provider.Response
	}{
		{
			// constructed from the OpenAI API reference example response, not recorded
			"openai_chat_constructed.json",
			provider.Response{Content: "Hello! How can I assist you today?", FinishReason: "stop",
				Usage: provider.Usage{PromptTokens: 19, CompletionTokens: 10, TotalTokens: 29}},
		},
		{
			// recorded from Groq
			"groq_chat_recorded.json",
			provider.Response{Content: "الأحمر، والأزرق، والأصفر ", FinishReason: "stop",
				Usage: provider.Usage{PromptTokens: 16, CompletionTokens: 10, TotalTokens: 26}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			a, _ := serve(t, "application/json", fixture(t, tc.file))
			resp, err := a.Chat(t.Context(), provider.Request{Model: "m"})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			resp.Usage.Timings = nil
			if resp != tc.want {
				t.Errorf("Chat = %+v, want %+v", resp, tc.want)
			}
		})
	}
}

func TestStreamRequestBody(t *testing.T) {
	t.Parallel()
	a, got := serve(t, "text/event-stream", []byte("data: [DONE]\n\n"))
	temp, maxTokens, seed := 0.0, 64, int64(7)
	req := provider.Request{
		Model:       "llama-test",
		Messages:    []provider.Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"}},
		Temperature: &temp,
		MaxTokens:   &maxTokens,
		Stop:        []string{"\n\n"},
		Seed:        &seed,
	}
	if err := a.Stream(t.Context(), req, func(provider.Event) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	want := `{"model":"llama-test","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],` +
		`"temperature":0,"max_completion_tokens":64,"stop":["\n\n"],"seed":7,"stream":true,"stream_options":{"include_usage":true}}`
	if string(*got) != want {
		t.Errorf("request body:\n%s\nwant:\n%s", *got, want)
	}
}

func TestChatRequestBodyLeavesOutUnsetFields(t *testing.T) {
	t.Parallel()
	a, got := serve(t, "application/json", fixture(t, "openai_chat_constructed.json"))
	if _, err := a.Chat(t.Context(), provider.Request{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if want := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`; string(*got) != want {
		t.Errorf("request body = %s, want %s", *got, want)
	}
}

func TestStreamReadsAChunkLargerThan64KiB(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 100<<10)
	body := append(wire{}.Delta(big), wire{}.End(provider.Usage{TotalTokens: 1}, "stop")...)
	a, _ := serve(t, "text/event-stream", body)
	evs, err := collect(t, a)
	if err != nil {
		t.Fatalf("Stream with a 100 KiB chunk: %v", err)
	}
	if got := summarize(evs).Text; got != big {
		t.Errorf("got %d bytes of text, want %d", len(got), len(big))
	}
}

func TestStreamToleratesCommentsCRLFAndNoSpace(t *testing.T) {
	t.Parallel()
	body := ": keep-alive\r\n\r\n" +
		`data:{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}` + "\r\n\r\n" +
		": another keep-alive\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"b"},"finish_reason":"stop"}]}` + "\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	a, _ := serve(t, "text/event-stream", []byte(body))
	evs, err := collect(t, a)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if s := summarize(evs); s.Text != "ab" || !slices.Equal(s.Finish, []string{"stop"}) {
		t.Errorf("got %+v, want text ab and finish stop", s)
	}
}

func TestStreamAcceptsDoneWithoutTheClosingBlankLine(t *testing.T) {
	t.Parallel()
	body := string(wire{}.Delta("a")) + "data: [DONE]\n"
	a, _ := serve(t, "text/event-stream", []byte(body))
	if _, err := collect(t, a); err != nil {
		t.Fatalf("Stream: %v", err)
	}
}

func TestStreamWithoutDoneIsAnError(t *testing.T) {
	t.Parallel()
	a, _ := serve(t, "text/event-stream", wire{}.Delta("cut off"))
	evs, err := collect(t, a)
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.KindUpstreamUnavailable {
		t.Fatalf("err = %v, want kind upstream_unavailable", err)
	}
	if got := summarize(evs).Text; got != "cut off" {
		t.Errorf("text before the error = %q, want %q", got, "cut off")
	}
}

func TestStreamErrorEventsAreProviderErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"openai error object": `data: {"error":{"message":"overloaded","type":"server_error"}}` + "\n\n",
		"groq x_groq.error":   `data: {"choices":[],"x_groq":{"id":"req_1","error":"stream stopped early"}}` + "\n\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a, _ := serve(t, "text/event-stream", append(wire{}.Delta("a"), body...))
			_, err := collect(t, a)
			var pe *provider.Error
			if !errors.As(err, &pe) || pe.Kind != provider.KindUpstreamUnavailable || pe.Provider != "test" {
				t.Errorf("err = %v, want a provider error of kind upstream_unavailable", err)
			}
		})
	}
}

func TestStreamReportsUsageOnceWhenBothShapesArrive(t *testing.T) {
	t.Parallel()
	body := string(chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"x_groq": map[string]any{"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}}})) +
		string(chunk(map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}})) +
		"data: [DONE]\n\n"
	a, _ := serve(t, "text/event-stream", []byte(body))
	evs, err := collect(t, a)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := summarize(evs).Usage; len(got) != 1 || got[0].TotalTokens != 7 {
		t.Errorf("usage events = %+v, want exactly one with total 7", got)
	}
}

func dur(d *time.Duration) string {
	if d == nil {
		return "unset"
	}
	return d.String()
}

func timings(u *provider.Usage) string {
	if u == nil || u.Timings == nil {
		return "none"
	}
	return fmt.Sprintf("queue=%s load=%s prefill=%s decode=%s", dur(u.Timings.Queue), dur(u.Timings.Load), dur(u.Timings.Prefill), dur(u.Timings.Decode))
}

func TestGroqTimingsAreConvertedFromSeconds(t *testing.T) {
	t.Parallel()
	cases := []struct{ file, want string }{
		// recorded from Groq
		{"groq_stream_recorded.sse", "queue=83.29193ms load=unset prefill=1.039105ms decode=7.396903ms"},
		// constructed from the OpenAI API reference: OpenAI reports no timings
		{"openai_stream_constructed.sse", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			a, _ := serve(t, "text/event-stream", fixture(t, tc.file))
			var u *provider.Usage
			err := a.Stream(t.Context(), provider.Request{Model: "m"}, func(e provider.Event) error {
				if e.Usage != nil {
					u = e.Usage
				}
				return nil
			})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if got := timings(u); got != tc.want {
				t.Errorf("timings = %s, want %s", got, tc.want)
			}
		})
	}

	t.Run("groq_chat_recorded.json", func(t *testing.T) {
		t.Parallel()
		a, _ := serve(t, "application/json", fixture(t, "groq_chat_recorded.json"))
		resp, err := a.Chat(t.Context(), provider.Request{Model: "m"})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if got, want := timings(&resp.Usage), "queue=88.860954ms load=unset prefill=1.035525ms decode=7.399842ms"; got != want {
			t.Errorf("timings = %s, want %s", got, want)
		}
	})
}

func TestTheKeyIsSentAndNeverPrinted(t *testing.T) {
	t.Parallel()
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API Key","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(srv.Close)
	a := openaicompat.New("groq", srv.URL, testKey, srv.Client())

	_, err := a.Chat(t.Context(), provider.Request{Model: "m"})
	if auth != "Bearer "+string(testKey) {
		t.Errorf("Authorization header = %q, want the bearer key", auth)
	}

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	log.Error("call failed", "err", err, "adapter", a)
	printed := map[string]string{
		"error":       fmt.Sprint(err),
		"%+v adapter": fmt.Sprintf("%+v", a),
		"%#v adapter": fmt.Sprintf("%#v", a),
		"JSON log":    logs.String(),
	}
	for where, out := range printed {
		if strings.Contains(out, string(testKey)) {
			t.Errorf("%s printed the API key: %s", where, out)
		}
	}
}

func TestReasoningDeltasArriveAsProgressEvents(t *testing.T) {
	t.Parallel()
	// recorded from Groq (gpt-oss): 29 reasoning deltas and no content
	a, _ := serve(t, "text/event-stream", fixture(t, "groq_reasoning_stream_recorded.sse"))
	evs, err := collect(t, a)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	progress := 0
	for _, e := range evs {
		if e.Progress {
			progress++
			if e.Delta != "" || e.Usage != nil || e.FinishReason != "" {
				t.Errorf("a progress event carries content: %+v", e)
			}
		}
	}
	if progress != 29 {
		t.Errorf("got %d progress events, want 29 (one per reasoning delta)", progress)
	}
}
