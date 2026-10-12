//go:build live

// Live calls to a local Ollama. Run with:
//
//	go test -tags live -run TestLiveOllama -v ./internal/provider/ollama/
package ollama_test

import (
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/provider/ollama"
)

func liveBase() string { return cmp.Or(os.Getenv("OLLAMA_BASE_URL"), "http://localhost:11434") }

func liveModel() string { return cmp.Or(os.Getenv("OLLAMA_LIVE_MODEL"), "llama3.2:3b") }

func rawChat(t *testing.T, body map[string]any) []byte {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, liveBase()+"/api/chat", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("Ollama is not reachable at %s: %v", liveBase(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	return out
}

func unload(t *testing.T) {
	t.Helper()
	rawChat(t, map[string]any{"model": liveModel(), "messages": []any{}, "keep_alive": 0})
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func msp(d *time.Duration) float64 {
	if d == nil {
		return -1
	}
	return ms(*d)
}

func TestLiveOllamaTTFT(t *testing.T) {
	a := ollama.New("ollama", liveBase(), provider.NewHTTPClient(5*time.Second, 120*time.Second))
	maxTokens := 16
	req := provider.Request{
		Model:     liveModel(),
		Messages:  []provider.Message{{Role: "user", Content: "Name one primary colour. One word."}},
		MaxTokens: &maxTokens,
	}
	measure := func(label string, run int) {
		var headers, first time.Duration
		var u provider.Usage
		start := time.Now()
		ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
			GotFirstResponseByte: func() { headers = time.Since(start) },
		})
		err := a.Stream(ctx, req, func(e provider.Event) error {
			if e.Delta != "" && first == 0 {
				first = time.Since(start)
			}
			if e.Usage != nil {
				u = *e.Usage
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%s run %d: %v", label, run, err)
		}
		var load, prefill, decode float64 = -1, -1, -1
		if u.Timings != nil {
			load, prefill, decode = msp(u.Timings.Load), msp(u.Timings.Prefill), msp(u.Timings.Decode)
		}
		t.Logf("%-5s %-3d %10.0f %14.0f %8.0f %9.0f %11.1f %10.1f %7d", label, run, ms(headers), ms(first), ms(first-headers),
			load, prefill, decode, u.PromptTokens)
	}

	t.Logf("model %s", liveModel())
	t.Logf("%-5s %-3s %10s %14s %8s %9s %11s %10s %7s", "model", "run", "headers_ms", "first_token_ms", "gap_ms",
		"load_ms", "prefill_ms", "decode_ms", "prompt")
	measure("warm", 0) // loads the model; not reported as warm
	for run := 1; run <= 5; run++ {
		measure("warm", run)
	}
	for run := 1; run <= 3; run++ {
		unload(t)
		measure("cold", run)
	}
}

var createdAt = regexp.MustCompile(`"created_at":"[^"]*"`)

// TestLiveOllamaRecord writes the recorded fixtures when OLLAMA_RECORD=1.
func TestLiveOllamaRecord(t *testing.T) {
	if os.Getenv("OLLAMA_RECORD") != "1" {
		t.Skip("set OLLAMA_RECORD=1 to record fixtures")
	}
	body := map[string]any{
		"model":    liveModel(),
		"messages": []map[string]string{{"role": "user", "content": "Name three primary colours, comma separated."}},
		"options":  map[string]any{"num_predict": 24, "temperature": 0, "seed": 7},
	}
	for _, stream := range []bool{true, false} {
		body["stream"] = stream
		out := createdAt.ReplaceAll(rawChat(t, body), []byte(`"created_at":"2026-01-01T00:00:00Z"`))
		file := "testdata/ollama_chat_recorded.json"
		if stream {
			file = "testdata/ollama_stream_recorded.ndjson"
		}
		if err := os.WriteFile(file, out, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", file, len(out))
	}
}
