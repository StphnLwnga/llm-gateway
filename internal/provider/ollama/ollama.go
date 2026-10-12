// Package ollama is the provider adapter for Ollama's native chat API, which
// reports load, prefill and decode durations that its OpenAI-compatible API does not.
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/provider"
)

// maxLineSize bounds one NDJSON line, as in the OpenAI-compatible adapter.
const maxLineSize = 4 << 20

var errNoDone = errors.New(`stream ended before a line with "done": true`)

// Adapter calls one Ollama server.
type Adapter struct {
	name    string
	baseURL string
	client  *http.Client
}

// New returns an adapter for the Ollama server at baseURL, for example http://localhost:11434.
func New(name, baseURL string, client *http.Client) *Adapter {
	return &Adapter{name: name, baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type options struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	NumPredict  *int     `json:"num_predict,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	Seed        *int64   `json:"seed,omitempty"`
}

type request struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Stream   bool      `json:"stream"`
	Options  *options  `json:"options,omitempty"`
}

// response is one NDJSON line, or the whole non-streamed body. Ollama leaves
// out a zero count or duration, so every metric is a pointer.
type response struct {
	Model   string `json:"model"`
	Message struct {
		Content string `json:"content"`
		// Thinking is not forwarded; it only marks progress.
		Thinking string `json:"thinking"`
	} `json:"message"`
	Done               bool   `json:"done"`
	DoneReason         string `json:"done_reason"`
	PromptEvalCount    *int   `json:"prompt_eval_count"`
	EvalCount          *int   `json:"eval_count"`
	LoadDuration       *int64 `json:"load_duration"`
	PromptEvalDuration *int64 `json:"prompt_eval_duration"`
	EvalDuration       *int64 `json:"eval_duration"`
	TotalDuration      *int64 `json:"total_duration"`
	Error              string `json:"error"`
}

func (a *Adapter) post(ctx context.Context, req provider.Request, stream bool) (*http.Response, error) {
	body := request{Model: req.Model, Messages: make([]message, len(req.Messages)), Stream: stream}
	for i, m := range req.Messages {
		body.Messages[i] = message(m)
	}
	o := options{Temperature: req.Temperature, TopP: req.TopP, NumPredict: req.MaxTokens, Stop: req.Stop, Seed: req.Seed}
	if o.Temperature != nil || o.TopP != nil || o.NumPredict != nil || o.Stop != nil || o.Seed != nil {
		body.Options = &o
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("provider %s: encode request: %w", a.name, err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/api/chat", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("provider %s: build request: %w", a.name, err)
	}
	hreq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(hreq)
	if err != nil {
		return nil, provider.TransportError(ctx, a.name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		return nil, provider.ResponseError(a.name, resp)
	}
	return resp, nil
}

// Chat sends a non-streamed request.
func (a *Adapter) Chat(ctx context.Context, req provider.Request) (provider.Response, error) {
	resp, err := a.post(ctx, req, false)
	if err != nil {
		return provider.Response{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	var r response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		if ctx.Err() != nil {
			return provider.Response{}, provider.TransportError(ctx, a.name, err)
		}
		return provider.Response{}, provider.MalformedError(a.name, err)
	}
	if err := a.lineError(r); err != nil {
		return provider.Response{}, err
	}
	if !r.Done {
		return provider.Response{}, provider.MalformedError(a.name, errors.New(`response has no "done": true`))
	}
	finish, err := a.finishReason(r.DoneReason)
	if err != nil {
		return provider.Response{}, err
	}
	return provider.Response{Content: r.Message.Content, Usage: usageOf(r), FinishReason: finish, Model: r.Model}, nil
}

// Stream sends a streamed request and calls fn once per event. The last line
// gives one event with the finish reason and the usage.
func (a *Adapter) Stream(ctx context.Context, req provider.Request, fn func(provider.Event) error) error {
	resp, err := a.post(ctx, req, true)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineSize)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r response
		if err := json.Unmarshal(line, &r); err != nil {
			return provider.MalformedError(a.name, err)
		}
		if err := a.lineError(r); err != nil {
			return err
		}
		if !r.Done {
			ev := provider.Event{Delta: r.Message.Content, Model: r.Model, Progress: r.Message.Content == "" && r.Message.Thinking != ""}
			if ev.Delta == "" && !ev.Progress {
				continue
			}
			if err := fn(ev); err != nil {
				return err
			}
			continue
		}
		finish, err := a.finishReason(r.DoneReason)
		if err != nil {
			return err
		}
		u := usageOf(r)
		return fn(provider.Event{Delta: r.Message.Content, FinishReason: finish, Usage: &u, Model: r.Model})
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return provider.MalformedError(a.name, fmt.Errorf("line larger than %d bytes", maxLineSize))
		}
		return provider.TransportError(ctx, a.name, err)
	}
	if ctx.Err() != nil {
		return provider.TransportError(ctx, a.name, ctx.Err())
	}
	return &provider.Error{Provider: a.name, StatusCode: resp.StatusCode, Kind: provider.KindUpstreamUnavailable, Err: errNoDone}
}

func (a *Adapter) lineError(r response) error {
	if r.Error == "" {
		return nil
	}
	return &provider.Error{Provider: a.name, StatusCode: http.StatusOK, Kind: provider.KindUpstreamUnavailable, Err: errors.New(r.Error)}
}

// finishReason maps done_reason to the gateway's OpenAI finish reasons.
// "load" and "unload" answer a request with no messages: the model was
// loaded or unloaded and nothing was generated.
func (a *Adapter) finishReason(doneReason string) (string, error) {
	switch doneReason {
	case "stop", "length":
		return doneReason, nil
	case "load", "unload":
		return "", &provider.Error{Provider: a.name, StatusCode: http.StatusOK, Kind: provider.KindBadRequest,
			Err: fmt.Errorf("done_reason %q: the request had no messages, so nothing was generated", doneReason)}
	default:
		return "", &provider.Error{Provider: a.name, StatusCode: http.StatusOK, Kind: provider.KindUpstreamUnavailable,
			Err: fmt.Errorf("unexpected done_reason %q", doneReason)}
	}
}

// usageOf reads the final line's counts. A missing prompt_eval_count is
// unknown, not zero: a real prompt always has tokens. A missing eval_count is
// zero, because Ollama leaves out a zero count.
func usageOf(r response) provider.Usage {
	var u provider.Usage
	if r.EvalCount != nil {
		u.CompletionTokens = *r.EvalCount
	}
	if r.PromptEvalCount != nil {
		u.PromptTokens = *r.PromptEvalCount
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	} else {
		u.PromptTokensUnknown = true
	}
	t := provider.Timings{Load: nanos(r.LoadDuration), Prefill: nanos(r.PromptEvalDuration), Decode: nanos(r.EvalDuration), Total: nanos(r.TotalDuration)}
	if t != (provider.Timings{}) {
		u.Timings = &t
	}
	return u
}

// nanos converts an Ollama duration, which is in nanoseconds.
func nanos(n *int64) *time.Duration {
	if n == nil {
		return nil
	}
	d := time.Duration(*n)
	return &d
}
