package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/StphnLwnga/llm-gateway/internal/fakeupstream"
	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/provider/openaicompat"
)

// serve runs h on a plain http.Server, whose Close does not wait for handlers,
// so a leaked handler fails a test instead of hanging the run.
func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type resolver map[string][]httpapi.Target

func (r resolver) Resolve(alias string) []httpapi.Target { return r[alias] }

type setup struct {
	upstreamModel string
	upstream      string            // base URL of the upstream; the fake upstream when empty
	provider      provider.Provider // used instead of an HTTP adapter when set
	hold          time.Duration
	heartbeat     time.Duration
	firstByte     time.Duration
	firstToken    time.Duration
	stall         time.Duration
	maxMax        *int
}

type gateway struct {
	url  string
	logs *syncBuffer
}

func start(t *testing.T, s setup) gateway {
	t.Helper()
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&s.hold, 3*time.Second)
	def(&s.heartbeat, 100*time.Millisecond)
	def(&s.firstByte, 5*time.Second)
	def(&s.firstToken, 5*time.Second)
	def(&s.stall, 5*time.Second)
	p := s.provider
	if p == nil {
		base := s.upstream
		if base == "" {
			base = serve(t, fakeupstream.New())
		}
		p = openaicompat.New("fake", base+"/v1", "test-key", provider.NewHTTPClient(time.Second, s.firstByte))
	}
	p = &provider.Watchdog{Name: "fake", Inner: p, FirstEvent: s.firstToken, Stall: s.stall}
	logs := &syncBuffer{}
	h := httpapi.New(slog.New(slog.NewJSONHandler(logs, nil)), httpapi.Deps{
		Routes: resolver{"chat": {{ProviderName: "fake", Provider: p, UpstreamModel: s.upstreamModel, MaxMaxTokens: s.maxMax}}},
		Chat:   httpapi.ChatConfig{StatusHold: s.hold, HeartbeatInterval: s.heartbeat, MaxBodyBytes: 4 << 10},
	})
	return gateway{url: serve(t, h), logs: logs}
}

func body(stream bool, extra string) string {
	return `{"model":"chat","stream":` + map[bool]string{true: "true", false: "false"}[stream] +
		`,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"say something short"}]` + extra + `}`
}

func post(ctx context.Context, t *testing.T, g gateway, b string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url+"/v1/chat/completions", strings.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

type line struct {
	text string
	at   time.Duration
}

// lines reads the SSE body line by line, with the time each line arrived.
func lines(resp *http.Response, began time.Time) []line {
	var out []line
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() != "" {
			out = append(out, line{sc.Text(), time.Since(began)})
		}
	}
	return out
}

func contentOf(ls []line) string {
	var b strings.Builder
	for _, l := range ls {
		var c struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if p, ok := strings.CutPrefix(l.text, "data: "); ok && json.Unmarshal([]byte(p), &c) == nil && len(c.Choices) == 1 {
			b.WriteString(c.Choices[0].Delta.Content)
		}
	}
	return b.String()
}

func count(ls []line, prefix string) int {
	n := 0
	for _, l := range ls {
		if strings.HasPrefix(l.text, prefix) {
			n++
		}
	}
	return n
}

func errorBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil || env.Error == nil {
		t.Fatalf("body is not an OpenAI error: %v", err)
	}
	return env.Error
}

// logLine returns the request's "chat completion" log record once it is written.
func logLine(t *testing.T, g gateway) map[string]any {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, l := range strings.Split(g.logs.String(), "\n") {
			if strings.Contains(l, `"msg":"chat completion"`) {
				var m map[string]any
				_ = json.Unmarshal([]byte(l), &m)
				return m
			}
		}
	}
	t.Fatalf("no chat completion log line:\n%s", g.logs.String())
	return nil
}

func TestAFastStreamIs200WithNoHeartbeats(t *testing.T) {
	t.Parallel()
	g := start(t, setup{upstreamModel: "fake?tokens=3"})
	began := time.Now()
	resp := post(t.Context(), t, g, body(true, ""))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, Content-Type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Request-Id") == "" || resp.Header.Get("X-LLM-Gateway-Provider") != "fake" {
		t.Errorf("headers = %v", resp.Header)
	}
	ls := lines(resp, began)
	if got := contentOf(ls); got != "The quick brown" {
		t.Errorf("content = %q", got)
	}
	if count(ls, ": heartbeat") != 0 {
		t.Error("a fast stream sent heartbeats")
	}
	last := ls[len(ls)-1].text
	usage := ls[len(ls)-2].text
	if last != "data: [DONE]" || !strings.Contains(usage, `"choices":[]`) || !strings.Contains(usage, `"completion_tokens":3`) {
		t.Errorf("the stream must end with the usage chunk and [DONE]; last lines: %q, %q", usage, last)
	}
	if !strings.Contains(ls[0].text, `"model":"chat"`) || !strings.Contains(ls[0].text, `"object":"chat.completion.chunk"`) ||
		!strings.Contains(ls[0].text, `"role":"assistant"`) {
		t.Errorf("first chunk = %s", ls[0].text)
	}
	if m := logLine(t, g); m["committed_by"] != "first_token" || m["truncated"] != false || m["status"] != 200.0 {
		t.Errorf("log = %v", m)
	}
}

func TestAPastTheHoldSends200AndHeartbeatsThenTokens(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"fake?ttfb=600ms&tokens=2", "fake?first_token=600ms&tokens=2"} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			g := start(t, setup{upstreamModel: model, hold: 200 * time.Millisecond})
			began := time.Now()
			resp := post(t.Context(), t, g, body(true, ""))
			defer func() { _ = resp.Body.Close() }()
			if d := time.Since(began); resp.StatusCode != 200 || d < 150*time.Millisecond || d > 500*time.Millisecond {
				t.Errorf("status %d after %v, want 200 at the 200ms hold", resp.StatusCode, d)
			}
			ls := lines(resp, began)
			if ls[0].text != ": heartbeat" || count(ls, ": heartbeat") < 2 {
				t.Errorf("want heartbeats first, got %v", ls[:min(3, len(ls))])
			}
			if contentOf(ls) != "The quick" || ls[len(ls)-1].text != "data: [DONE]" {
				t.Errorf("content %q, last %q", contentOf(ls), ls[len(ls)-1].text)
			}
			if m := logLine(t, g); m["committed_by"] != "hold_elapsed" {
				t.Errorf("committed_by = %v, want hold_elapsed", m["committed_by"])
			}
		})
	}
}

func TestUpstreamErrorsBeforeTheCommitGetARealStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model, retryAfter, code string
		status                  int
	}{
		{"fake?status=500", "", "upstream_unavailable", 502},
		{"fake?status=429&retry_after=7", "7", "rate_limit_exceeded", 429},
		{"fake?status=401", "", "upstream_auth", 502},
		{"fake?status=400", "", "", 400},
	}
	for _, tc := range cases {
		for _, stream := range []bool{true, false} {
			t.Run(tc.model, func(t *testing.T) {
				t.Parallel()
				g := start(t, setup{upstreamModel: tc.model})
				resp := post(t.Context(), t, g, body(stream, ""))
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode != tc.status || resp.Header.Get("Retry-After") != tc.retryAfter {
					t.Errorf("status %d Retry-After %q, want %d %q", resp.StatusCode, resp.Header.Get("Retry-After"), tc.status, tc.retryAfter)
				}
				e := errorBody(t, resp)
				if code, _ := e["code"].(string); code != tc.code {
					t.Errorf("code = %v, want %q", e["code"], tc.code)
				}
				if msg, _ := e["message"].(string); strings.Contains(msg, "Sorry about that") || strings.Contains(msg, "Rate limit reached") {
					t.Errorf("the upstream's own message reached the client: %q", msg)
				}
			})
		}
	}
}

func TestErrorsAfterTheCommitEndTheStreamWithAnErrorEvent(t *testing.T) {
	t.Parallel()
	cases := map[string]setup{
		"stall":     {upstreamModel: "fake?tokens=5&stall_after=2", stall: 300 * time.Millisecond},
		"drop":      {upstreamModel: "fake?tokens=5&drop_after=2"},
		"malformed": {upstreamModel: "fake?tokens=5&malformed_after=2"},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := start(t, s)
			resp := post(t.Context(), t, g, body(true, ""))
			defer func() { _ = resp.Body.Close() }()
			ls := lines(resp, time.Now())
			if resp.StatusCode != 200 || contentOf(ls) != "The quick" {
				t.Fatalf("status %d, content %q", resp.StatusCode, contentOf(ls))
			}
			last := ls[len(ls)-1].text
			if !strings.HasPrefix(last, `data: {"error":{`) || count(ls, "data: [DONE]") != 0 {
				t.Errorf("want an error event and no [DONE]; last line %q", last)
			}
			if m := logLine(t, g); m["truncated"] != true || m["error_kind"] == nil {
				t.Errorf("log = %v, want truncated with an error kind", m)
			}
		})
	}
}

func TestFirstTokenTimeout(t *testing.T) {
	t.Parallel()
	t.Run("before the hold gives 504", func(t *testing.T) {
		t.Parallel()
		g := start(t, setup{upstreamModel: "fake?first_token=2s&tokens=1", firstToken: 200 * time.Millisecond, hold: time.Second})
		resp := post(t.Context(), t, g, body(true, ""))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 504 || errorBody(t, resp)["code"] != "upstream_timeout" {
			t.Errorf("status = %d, want 504 upstream_timeout", resp.StatusCode)
		}
	})
	t.Run("after the hold gives an error event", func(t *testing.T) {
		t.Parallel()
		g := start(t, setup{upstreamModel: "fake?first_token=2s&tokens=1", firstToken: 400 * time.Millisecond, hold: 100 * time.Millisecond})
		resp := post(t.Context(), t, g, body(true, ""))
		defer func() { _ = resp.Body.Close() }()
		ls := lines(resp, time.Now())
		last := ls[len(ls)-1].text
		if resp.StatusCode != 200 || count(ls, ": heartbeat") == 0 || !strings.Contains(last, "upstream_timeout") {
			t.Errorf("status %d, lines %v", resp.StatusCode, ls)
		}
	})
}

// reasoner is a provider that reasons (progress events) before it answers.
type reasoner struct{ steps int }

func (r reasoner) Chat(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (r reasoner) Stream(ctx context.Context, _ provider.Request, fn func(provider.Event) error) error {
	for range r.steps {
		select {
		case <-time.After(60 * time.Millisecond):
		case <-ctx.Done():
			return provider.TransportError(ctx, "fake", ctx.Err())
		}
		if err := fn(provider.Event{Progress: true}); err != nil {
			return err
		}
	}
	u := provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}
	return fn(provider.Event{Delta: "Blue.", FinishReason: "stop", Usage: &u})
}

func TestProgressEventsKeepASlowReasoningStreamAlive(t *testing.T) {
	t.Parallel()
	// 10 steps x 60ms = 600ms of reasoning, past both 150ms deadlines
	g := start(t, setup{provider: reasoner{steps: 10}, firstToken: 150 * time.Millisecond, stall: 150 * time.Millisecond, hold: 100 * time.Millisecond})
	resp := post(t.Context(), t, g, body(true, ""))
	defer func() { _ = resp.Body.Close() }()
	ls := lines(resp, time.Now())
	if contentOf(ls) != "Blue." || ls[len(ls)-1].text != "data: [DONE]" {
		t.Errorf("lines = %v", ls)
	}
}

func TestRequestsTheGatewayRejects(t *testing.T) {
	t.Parallel()
	ten := 10
	g := start(t, setup{upstreamModel: "fake", maxMax: &ten})
	cases := []struct {
		name, body, param, code string
		status                  int
	}{
		{"unknown alias", `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`, "model", "model_not_found", 404},
		{"above the route maximum", body(true, `,"max_completion_tokens":50`), "max_completion_tokens", "", 400},
		{"tools", body(true, `,"tools":[{"type":"function"}]`), "tools", "", 400},
		{"response_format", body(false, `,"response_format":{"type":"json_object"}`), "response_format", "", 400},
		{"logprobs", body(false, `,"logprobs":true`), "logprobs", "", 400},
		{"n of 2", body(false, `,"n":2`), "n", "", 400},
		{"limits that disagree", body(false, `,"max_tokens":5,"max_completion_tokens":6`), "max_completion_tokens", "", 400},
		{"an image part", `{"model":"chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, "messages[0].content", "", 400},
		{"a tool message", `{"model":"chat","messages":[{"role":"tool","content":"x"}]}`, "messages[0].role", "", 400},
		{"no messages", `{"model":"chat","messages":[]}`, "messages", "", 400},
		{"not JSON", `{"model":`, "", "", 400},
		{"too large", `{"model":"chat","messages":[{"role":"user","content":"` + strings.Repeat("x", 8<<10) + `"}]}`, "", "", 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := post(t.Context(), t, g, tc.body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			e := errorBody(t, resp)
			if p, _ := e["param"].(string); p != tc.param {
				t.Errorf("param = %v, want %q", e["param"], tc.param)
			}
			if c, _ := e["code"].(string); c != tc.code {
				t.Errorf("code = %v, want %q", e["code"], tc.code)
			}
		})
	}
}

func TestNonStreamJoinsTheStreamIntoTheOpenAIShape(t *testing.T) {
	t.Parallel()
	// The upstream replays the recorded Groq stream; the recorded non-stream
	// answer from the same request is the reference shape.
	stream, err := os.ReadFile("../provider/openaicompat/testdata/groq_stream_recorded.sse")
	if err != nil {
		t.Fatal(err)
	}
	upstream := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(stream)
	}))
	g := start(t, setup{upstreamModel: "allam-2-7b", upstream: upstream})
	resp := post(t.Context(), t, g, body(false, ""))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, Content-Type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	ref, err := os.ReadFile("../provider/openaicompat/testdata/groq_chat_recorded.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(ref, &want); err != nil {
		t.Fatal(err)
	}

	if got["object"] != want["object"] || got["model"] != "chat" {
		t.Errorf("object, model = %v, %v; want %v, chat", got["object"], got["model"], want["object"])
	}
	if id, _ := got["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("id = %v", got["id"])
	}
	if _, ok := got["created"].(float64); !ok {
		t.Errorf("created = %v", got["created"])
	}
	gc := got["choices"].([]any)[0].(map[string]any)
	wc := want["choices"].([]any)[0].(map[string]any)
	for _, k := range []string{"index", "logprobs", "finish_reason"} {
		if gc[k] != wc[k] {
			t.Errorf("choices[0].%s = %v, want %v", k, gc[k], wc[k])
		}
	}
	gm, wm := gc["message"].(map[string]any), wc["message"].(map[string]any)
	if gm["role"] != wm["role"] || gm["content"] != wm["content"] {
		t.Errorf("message = %v, want %v", gm, wm)
	}
	gu, wu := got["usage"].(map[string]any), want["usage"].(map[string]any)
	for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if gu[k] != wu[k] {
			t.Errorf("usage.%s = %v, want %v", k, gu[k], wu[k])
		}
	}
}

func TestANonStreamAnswerHasNoTotalTimeLimit(t *testing.T) {
	t.Parallel()
	// 10 tokens 100ms apart: about 1s in total, five times the 200ms first-byte timeout.
	g := start(t, setup{upstreamModel: "fake?tokens=10&tpot=100ms", firstByte: 200 * time.Millisecond, stall: 500 * time.Millisecond})
	resp := post(t.Context(), t, g, body(false, ""))
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != 200 {
		t.Fatalf("status %d, err %v", resp.StatusCode, err)
	}
	if want := "The quick brown fox jumps over the lazy dog."; got.Choices[0].Message.Content != want {
		t.Errorf("content = %q, want %q", got.Choices[0].Message.Content, want)
	}
}

func TestANonStreamErrorMidGenerationGetsARealStatus(t *testing.T) {
	t.Parallel()
	g := start(t, setup{upstreamModel: "fake?tokens=5&stall_after=2", stall: 200 * time.Millisecond})
	resp := post(t.Context(), t, g, body(false, ""))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 504 {
		t.Errorf("status = %d, want 504", resp.StatusCode)
	}
}

// TestAClientDisconnectCancelsTheUpstream is not parallel: goleak compares
// every goroutine in the process with a snapshot.
func TestAClientDisconnectCancelsTheUpstream(t *testing.T) {
	g := start(t, setup{upstreamModel: "fake?tokens=100&tpot=50ms"})
	snapshot := goleak.IgnoreCurrent()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url+"/v1/chat/completions", strings.NewReader(body(true, "")))
	if err != nil {
		t.Fatal(err)
	}
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

	if m := logLine(t, g); m["client_gone"] != true {
		t.Errorf("log = %v, want client_gone", m)
	}
	goleak.VerifyNone(t, snapshot)
}
