package fakeupstream_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/StphnLwnga/llm-gateway/internal/fakeupstream"
)

type server struct{ URL string }

// newServer starts the fake. http.Server.Close, unlike httptest.Server.Close,
// does not wait for handlers, so a leaked handler fails a test instead of
// hanging the run.
func newServer(t *testing.T) server {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: fakeupstream.New(), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return server{URL: "http://" + ln.Addr().String()}
}

func chatBody(model string, stream, includeUsage bool) string {
	req := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "say something short"}},
		"stream":   stream,
	}
	if includeUsage {
		req["stream_options"] = map[string]bool{"include_usage": true}
	}
	b, err := json.Marshal(req)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func post(ctx context.Context, t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

const (
	roleChunk  = `data: {"id":"chatcmpl-fake","object":"chat.completion.chunk","created":0,"model":"fake","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n"
	theChunk   = `data: {"id":"chatcmpl-fake","object":"chat.completion.chunk","created":0,"model":"fake","choices":[{"index":0,"delta":{"content":"The"},"finish_reason":null}]}` + "\n\n"
	quickChunk = `data: {"id":"chatcmpl-fake","object":"chat.completion.chunk","created":0,"model":"fake","choices":[{"index":0,"delta":{"content":" quick"},"finish_reason":null}]}` + "\n\n"
	brownChunk = `data: {"id":"chatcmpl-fake","object":"chat.completion.chunk","created":0,"model":"fake","choices":[{"index":0,"delta":{"content":" brown"},"finish_reason":null}]}` + "\n\n"
	stopChunk  = `data: {"id":"chatcmpl-fake","object":"chat.completion.chunk","created":0,"model":"fake","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	done       = "data: [DONE]\n\n"
)

func TestStreamWritesTheOpenAIWireFormat(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	resp := post(t.Context(), t, srv.URL, chatBody("fake?tokens=3", true, false))
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	want := roleChunk + theChunk + quickChunk + brownChunk + stopChunk + done
	if got := readAll(t, resp); got != want {
		t.Errorf("body:\n%s\nwant:\n%s", got, want)
	}
}

func TestStreamWithIncludeUsageAddsTheUsageChunk(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	resp := post(t.Context(), t, srv.URL, chatBody("fake?tokens=1", true, true))
	defer func() { _ = resp.Body.Close() }()
	want := `data: {"id":"chatcmpl-fake","object":"chat.completion.chunk","created":0,"model":"fake","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}` + "\n\n" +
		`data: {"id":"chatcmpl-fake","object":"chat.completion.chunk","created":0,"model":"fake","choices":[{"index":0,"delta":{"content":"The"},"finish_reason":null}],"usage":null}` + "\n\n" +
		`data: {"id":"chatcmpl-fake","object":"chat.completion.chunk","created":0,"model":"fake","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}` + "\n\n" +
		`data: {"id":"chatcmpl-fake","object":"chat.completion.chunk","created":0,"model":"fake","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}` + "\n\n" +
		done
	if got := readAll(t, resp); got != want {
		t.Errorf("body:\n%s\nwant:\n%s", got, want)
	}
}

func TestChatWritesTheOpenAIWireFormat(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	resp := post(t.Context(), t, srv.URL, chatBody("fake?tokens=12", false, false))
	defer func() { _ = resp.Body.Close() }()
	want := `{"id":"chatcmpl-fake","object":"chat.completion","created":0,"model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"The quick brown fox jumps over the lazy dog.The quick"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":12,"total_tokens":15}}` + "\n"
	if got := readAll(t, resp); got != want {
		t.Errorf("body:\n%s\nwant:\n%s", got, want)
	}
}

func TestMaxTokensCutsTheAnswerWithFinishReasonLength(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	resp := post(t.Context(), t, srv.URL, `{"model":"fake?tokens=10","max_tokens":2,"messages":[]}`)
	defer func() { _ = resp.Body.Close() }()
	want := `{"id":"chatcmpl-fake","object":"chat.completion","created":0,"model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"The quick"},"finish_reason":"length"}],"usage":{"prompt_tokens":0,"completion_tokens":2,"total_tokens":2}}` + "\n"
	if got := readAll(t, resp); got != want {
		t.Errorf("body:\n%s\nwant:\n%s", got, want)
	}
}

func TestMaxCompletionTokensWinsOverMaxTokens(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	resp := post(t.Context(), t, srv.URL, `{"model":"fake?tokens=10","max_completion_tokens":1,"max_tokens":5,"messages":[]}`)
	defer func() { _ = resp.Body.Close() }()
	want := `{"id":"chatcmpl-fake","object":"chat.completion","created":0,"model":"fake","choices":[{"index":0,"message":{"role":"assistant","content":"The"},"finish_reason":"length"}],"usage":{"prompt_tokens":0,"completion_tokens":1,"total_tokens":1}}` + "\n"
	if got := readAll(t, resp); got != want {
		t.Errorf("body:\n%s\nwant:\n%s", got, want)
	}
}

func TestModelsListsTheFakeModel(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	want := `{"data":[{"created":0,"id":"fake","object":"model","owned_by":"llm-gateway"}],"object":"list"}` + "\n"
	if got := readAll(t, resp); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

// events reads SSE data lines as they arrive and records when each arrived.
type event struct {
	data string
	at   time.Duration
}

func events(t *testing.T, resp *http.Response, began time.Time) ([]event, error) {
	t.Helper()
	var out []event
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, event{data: line, at: time.Since(began)})
		}
	}
	return out, sc.Err()
}

func contents(evs []event) []string {
	var out []string
	for _, e := range evs {
		var c struct {
			Choices []struct {
				Delta struct {
					Content *string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(e.data), &c) == nil && len(c.Choices) == 1 && c.Choices[0].Delta.Content != nil && *c.Choices[0].Delta.Content != "" {
			out = append(out, *c.Choices[0].Delta.Content)
		}
	}
	return out
}

func TestTTFBDelaysTheHeaders(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	began := time.Now()
	resp := post(t.Context(), t, srv.URL, chatBody("fake?ttfb=300ms&tokens=1", true, false))
	defer func() { _ = resp.Body.Close() }()
	if d := time.Since(began); d < 300*time.Millisecond {
		t.Errorf("headers after %v, want 300ms or later", d)
	}
	readAll(t, resp)
}

func TestFirstTokenSendsHeadersAtOnceThenWaits(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	began := time.Now()
	resp := post(t.Context(), t, srv.URL, chatBody("fake?first_token=300ms&tokens=1", true, false))
	defer func() { _ = resp.Body.Close() }()
	if d := time.Since(began); d > 100*time.Millisecond {
		t.Errorf("headers after %v, want under 100ms", d)
	}
	evs, err := events(t, resp, began)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 {
		t.Fatal("got no chunks")
	}
	if evs[0].at < 300*time.Millisecond {
		t.Errorf("first chunk at %v, want 300ms or later", evs[0].at)
	}
}

func TestTPOTSpacesTheTokens(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	began := time.Now()
	resp := post(t.Context(), t, srv.URL, chatBody("fake?tokens=4&tpot=100ms", true, false))
	defer func() { _ = resp.Body.Close() }()
	evs, err := events(t, resp, began)
	if err != nil {
		t.Fatal(err)
	}
	var tokenTimes []time.Duration
	for _, e := range evs {
		if len(contents([]event{e})) == 1 {
			tokenTimes = append(tokenTimes, e.at)
		}
	}
	if len(tokenTimes) != 4 {
		t.Fatalf("got %d tokens, want 4", len(tokenTimes))
	}
	for i := 1; i < len(tokenTimes); i++ {
		if gap := tokenTimes[i] - tokenTimes[i-1]; gap < 90*time.Millisecond {
			t.Errorf("gap before token %d = %v, want about 100ms", i, gap)
		}
	}
	if spread := tokenTimes[3] - tokenTimes[0]; spread > time.Second {
		t.Errorf("4 tokens took %v, want about 300ms", spread)
	}
}

func TestTokensSetsTheCountAndTheUsage(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	resp := post(t.Context(), t, srv.URL, chatBody("fake?tokens=25", true, true))
	defer func() { _ = resp.Body.Close() }()
	evs, err := events(t, resp, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(contents(evs)); got != 25 {
		t.Errorf("got %d content chunks, want 25", got)
	}
	if !strings.Contains(evs[len(evs)-2].data, `"usage":{"prompt_tokens":3,"completion_tokens":25,"total_tokens":28}`) {
		t.Errorf("usage chunk = %s, want completion_tokens 25", evs[len(evs)-2].data)
	}
}

func TestStallAfterSendsNTokensThenHangs(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	began := time.Now()
	resp := post(ctx, t, srv.URL, chatBody("fake?tokens=5&stall_after=2", true, false))
	defer func() { _ = resp.Body.Close() }()
	evs, err := events(t, resp, began)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read ended with %v, want the client's deadline", err)
	}
	if got := contents(evs); len(got) != 2 {
		t.Errorf("got tokens %q before the stall, want 2", got)
	}
}

func TestDropAfterClosesTheConnectionMidStream(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	resp := post(t.Context(), t, srv.URL, chatBody("fake?tokens=5&drop_after=2", true, false))
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("read ended with %v, want io.ErrUnexpectedEOF (no clean end)", err)
	}
	if want := roleChunk + theChunk + quickChunk; string(b) != want {
		t.Errorf("body before the drop:\n%s\nwant:\n%s", b, want)
	}
}

func TestMalformedAfterSendsAnInvalidChunk(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	resp := post(t.Context(), t, srv.URL, chatBody("fake?tokens=5&malformed_after=1", true, false))
	defer func() { _ = resp.Body.Close() }()
	want := roleChunk + theChunk + "data: {\"id\":\n\n"
	if got := readAll(t, resp); got != want {
		t.Errorf("body:\n%s\nwant:\n%s", got, want)
	}
}

func TestChatFaults(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	t.Run("stall", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(chatBody("fake?stall_after=1", false, false)))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want the client's deadline", err)
		}
	})

	t.Run("drop", func(t *testing.T) {
		t.Parallel()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(chatBody("fake?drop_after=1", false, false)))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("got a response, want the connection closed before one")
		}
		if !errors.Is(err, io.EOF) {
			t.Errorf("err = %v, want EOF", err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		t.Parallel()
		resp := post(t.Context(), t, srv.URL, chatBody("fake?malformed_after=1", false, false))
		defer func() { _ = resp.Body.Close() }()
		var v any
		if err := json.NewDecoder(resp.Body).Decode(&v); err == nil {
			t.Error("body decoded, want malformed JSON")
		}
	})
}

func TestStatusReturnsAnOpenAIErrorBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model, retryAfter, body string
		status                  int
	}{
		{"fake?status=500", "", `{"error":{"message":"The server had an error while processing your request. Sorry about that!","type":"server_error","param":null,"code":null}}`, 500},
		{"fake?status=429&retry_after=7", "7", `{"error":{"message":"Rate limit reached for requests","type":"requests","param":null,"code":"rate_limit_exceeded"}}`, 429},
	}
	srv := newServer(t)
	for _, tc := range cases {
		for _, stream := range []bool{true, false} {
			t.Run(tc.model, func(t *testing.T) {
				t.Parallel()
				resp := post(t.Context(), t, srv.URL, chatBody(tc.model, stream, false))
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode != tc.status {
					t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
				}
				if got := resp.Header.Get("Retry-After"); got != tc.retryAfter {
					t.Errorf("Retry-After = %q, want %q", got, tc.retryAfter)
				}
				if got := readAll(t, resp); got != tc.body+"\n" {
					t.Errorf("body = %s, want %s", got, tc.body)
				}
			})
		}
	}
}

func TestInvalidRequestsAreRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body string
		status     int
		wantInBody string
	}{
		{"unknown directive", chatBody("fake?tokns=5", true, false), 400, `unknown directive \"tokns\"`},
		{"directive set twice", chatBody("fake?tokens=1&tokens=2", true, false), 400, "set more than once"},
		{"bad duration", chatBody("fake?ttfb=2", true, false), 400, "ttfb"},
		{"delay too long", chatBody("fake?tpot=11m", true, false), 400, "tpot"},
		{"negative count", chatBody("fake?tokens=-1", true, false), 400, "tokens"},
		{"fault after the last token", chatBody("fake?tokens=2&stall_after=3", true, false), 400, "stall_after=3 is more than tokens=2"},
		{"two faults", chatBody("fake?drop_after=1&malformed_after=2", true, false), 400, "at most one"},
		{"retry_after without status", chatBody("fake?retry_after=3", true, false), 400, "retry_after needs status"},
		{"status that is not an error", chatBody("fake?status=200", true, false), 400, "status"},
		{"unknown model", chatBody("gpt-4o", true, false), 404, "model_not_found"},
		{"invalid JSON", `{"model":`, 400, "invalid JSON body"},
	}
	srv := newServer(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := post(t.Context(), t, srv.URL, tc.body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			body := readAll(t, resp)
			var env struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil || env.Error.Type != "invalid_request_error" {
				t.Errorf("body %s is not an OpenAI invalid_request_error", body)
			}
			if !strings.Contains(body, tc.wantInBody) {
				t.Errorf("body %s does not contain %q", body, tc.wantInBody)
			}
		})
	}
}

// TestDelaysEndWhenTheClientGoesAway is not parallel: goleak compares every
// goroutine in the process with a snapshot.
func TestDelaysEndWhenTheClientGoesAway(t *testing.T) {
	models := []string{
		"fake?ttfb=10m",
		"fake?first_token=10m",
		"fake?tokens=3&tpot=10m",
		"fake?tokens=3&stall_after=1",
	}
	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			srv := newServer(t)
			client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			snapshot := goleak.IgnoreCurrent()

			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
				strings.NewReader(chatBody(model, true, false)))
			if err != nil {
				t.Fatal(err)
			}
			if resp, err := client.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			if ctx.Err() == nil {
				t.Fatal("the request finished before the client gave up; the delay did not apply")
			}

			goleak.VerifyNone(t, snapshot)
		})
	}
}
