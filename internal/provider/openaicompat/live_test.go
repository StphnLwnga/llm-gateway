//go:build live

// Live calls to Groq. Run with:
//
//	go test -tags live -run TestLiveGroq -v ./internal/provider/openaicompat/
//
// The key is read from GROQ_API_KEY in the repo's .env (gitignored) and is
// never printed.
package openaicompat_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/provider/openaicompat"
)

const groqBaseURL = "https://api.groq.com/openai/v1"

func groqKey(t *testing.T) config.Secret {
	t.Helper()
	f, err := os.Open("../../../.env")
	if err != nil {
		t.Skip("no .env in the repo root")
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "GROQ_API_KEY="); ok {
			return config.Secret(strings.Trim(v, `"'`))
		}
	}
	t.Skip("GROQ_API_KEY is not in .env")
	return ""
}

func groqGet(t *testing.T, key config.Secret, path string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, groqBaseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+string(key))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	return b
}

func TestLiveGroqModels(t *testing.T) {
	key := groqKey(t)
	var out struct {
		Data []struct {
			ID            string `json:"id"`
			OwnedBy       string `json:"owned_by"`
			Active        bool   `json:"active"`
			ContextWindow int    `json:"context_window"`
		} `json:"data"`
	}
	if err := json.Unmarshal(groqGet(t, key, "/models"), &out); err != nil {
		t.Fatal(err)
	}
	for _, m := range out.Data {
		t.Logf("%-50s owned_by=%-12s active=%v context=%d", m.ID, m.OwnedBy, m.Active, m.ContextWindow)
	}
}

func liveModel(t *testing.T) string {
	t.Helper()
	m := os.Getenv("GROQ_LIVE_MODEL")
	if m == "" {
		t.Skip("set GROQ_LIVE_MODEL to a model id from TestLiveGroqModels")
	}
	return m
}

// longPrompt is about 2,000 tokens, so prefill takes measurable time.
func longPrompt() string {
	para := "The gateway receives a chat request, checks the tenant key, applies the rate limit and the budget, " +
		"chooses a provider from the route, streams the answer back to the client as server-sent events, " +
		"and records the usage in the ledger so that billing and analytics agree on the same numbers. "
	return strings.Repeat(para, 40) + "\n\nReply with exactly one word: ok."
}

func TestLiveGroqTTFT(t *testing.T) {
	key, model := groqKey(t), liveModel(t)
	a := openaicompat.New("groq", groqBaseURL, key, provider.NewHTTPClient(5*time.Second, 60*time.Second))
	maxTokens := 16
	if v := os.Getenv("GROQ_LIVE_MAX_TOKENS"); v != "" {
		if _, err := fmt.Sscan(v, &maxTokens); err != nil {
			t.Fatalf("GROQ_LIVE_MAX_TOKENS: %v", err)
		}
	}
	prompts := []struct{ name, text string }{
		{"short", "Reply with exactly one word: ok."},
		{"long", longPrompt()},
	}
	t.Logf("%-6s %-3s %10s %14s %8s %8s", "prompt", "run", "headers_ms", "first_token_ms", "gap_ms", "prompt_tok")
	for _, p := range prompts {
		for run := 1; run <= 5; run++ {
			var headers, first time.Duration
			var u provider.Usage
			for attempt := 1; ; attempt++ {
				headers, first, u = 0, 0, provider.Usage{}
				start := time.Now()
				ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
					GotFirstResponseByte: func() { headers = time.Since(start) },
				})
				err := a.Stream(ctx, provider.Request{
					Model:     model,
					Messages:  []provider.Message{{Role: "user", Content: p.text}},
					MaxTokens: &maxTokens,
				}, func(e provider.Event) error {
					if e.Delta != "" && first == 0 {
						first = time.Since(start)
					}
					if e.Usage != nil {
						u = *e.Usage
					}
					return nil
				})
				var pe *provider.Error
				if errors.As(err, &pe) && pe.Kind == provider.KindRateLimited && attempt < 4 {
					wait := max(pe.RetryAfter, 5*time.Second)
					t.Logf("%s run %d: rate limited, waiting %v (Retry-After %v)", p.name, run, wait, pe.RetryAfter)
					time.Sleep(wait)
					continue
				}
				if err != nil {
					t.Fatalf("%s run %d: %v", p.name, run, err)
				}
				break
			}
			t.Logf("%-6s %-3d %10.0f %14.0f %8.0f %8d", p.name, run, ms(headers), ms(first), ms(first-headers), u.PromptTokens)
			time.Sleep(time.Second) // stay well inside the free plan's rate limit
		}
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

var normalizers = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`"id":"chatcmpl-[^"]*"`), `"id":"chatcmpl-normalized"`},
	{regexp.MustCompile(`"id":"req_[^"]*"`), `"id":"req_normalized"`},
	{regexp.MustCompile(`"created":\d+`), `"created":0`},
}

func normalize(b []byte) []byte {
	for _, n := range normalizers {
		b = n.re.ReplaceAll(b, []byte(n.with))
	}
	return b
}

// TestLiveGroqRecord writes the recorded fixtures when GROQ_RECORD=1.
func TestLiveGroqRecord(t *testing.T) {
	if os.Getenv("GROQ_RECORD") != "1" {
		t.Skip("set GROQ_RECORD=1 to record fixtures")
	}
	key, model := groqKey(t), liveModel(t)
	for _, stream := range []bool{true, false} {
		body := map[string]any{
			"model":                 model,
			"messages":              []map[string]string{{"role": "user", "content": "Name three primary colours, comma separated."}},
			"max_completion_tokens": 32,
			"temperature":           0,
			"seed":                  7,
		}
		chatFile, streamFile := "testdata/groq_chat_recorded.json", "testdata/groq_stream_recorded.sse"
		if strings.Contains(model, "gpt-oss") {
			chatFile, streamFile = "testdata/groq_reasoning_chat_recorded.json", "testdata/groq_reasoning_stream_recorded.sse"
		}
		file := chatFile
		if stream {
			body["stream"] = true
			body["stream_options"] = map[string]bool{"include_usage": true}
			file = streamFile
		}
		b, _ := json.Marshal(body)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, groqBaseURL+"/chat/completions", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+string(key))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("record stream=%v: status %d, err %v", stream, resp.StatusCode, err)
		}
		header := fmt.Sprintf(": recorded from Groq (%s) on %s; ids and timestamps normalised\n\n", model, time.Now().UTC().Format("2006-01-02"))
		out := normalize(raw)
		if stream {
			out = append([]byte(header), out...)
		}
		if err := os.WriteFile(file, out, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", file, len(out))
	}
}
