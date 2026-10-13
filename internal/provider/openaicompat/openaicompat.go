// Package openaicompat is the provider adapter for OpenAI-compatible chat
// completions APIs: OpenAI and Groq.
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
)

// maxEventSize bounds one SSE line. bufio.Scanner's default of 64 KiB fails
// on a large chunk; a bound still stops one upstream from using unbounded memory.
const maxEventSize = 4 << 20

var errNoDone = errors.New("stream ended before data: [DONE]")

// Adapter calls one OpenAI-compatible API.
type Adapter struct {
	name    string
	baseURL string
	apiKey  config.Secret
	client  *http.Client
}

// New returns an adapter for the API at baseURL, for example https://api.groq.com/openai/v1.
func New(name, baseURL string, apiKey config.Secret, client *http.Client) *Adapter {
	return &Adapter{name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, client: client}
}

// Format prints the adapter without its key. fmt does not call the key's own
// redacting methods, because the field is unexported.
func (a *Adapter) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "openaicompat.Adapter{name: %s, baseURL: %s}", a.name, a.baseURL)
}

// LogValue implements slog.LogValuer.
func (a *Adapter) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", a.name), slog.String("base_url", a.baseURL))
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type request struct {
	Model               string         `json:"model"`
	Messages            []message      `json:"messages"`
	Temperature         *float64       `json:"temperature,omitempty"`
	TopP                *float64       `json:"top_p,omitempty"`
	MaxCompletionTokens *int           `json:"max_completion_tokens,omitempty"`
	Stop                []string       `json:"stop,omitempty"`
	Seed                *int64         `json:"seed,omitempty"`
	Stream              bool           `json:"stream,omitempty"`
	StreamOptions       *streamOptions `json:"stream_options,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	// Groq only, in seconds. Groq's reference gives no unit; its example
	// (0.463 for 556 tokens) and recorded streams only fit seconds.
	QueueTime      *float64 `json:"queue_time"`
	PromptTime     *float64 `json:"prompt_time"`
	CompletionTime *float64 `json:"completion_time"`
	TotalTime      *float64 `json:"total_time"`
}

func (u usage) toProvider() provider.Usage {
	out := provider.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
	t := provider.Timings{Queue: seconds(u.QueueTime), Prefill: seconds(u.PromptTime), Decode: seconds(u.CompletionTime), Total: seconds(u.TotalTime)}
	if t != (provider.Timings{}) {
		out.Timings = &t
	}
	return out
}

func seconds(s *float64) *time.Duration {
	if s == nil {
		return nil
	}
	d := time.Duration(math.Round(*s * float64(time.Second)))
	return &d
}

type apiError struct {
	Message string `json:"message"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
}

type chunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning"` // Groq reasoning models
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
	// Groq also reports usage, and an early stop, under x_groq.
	XGroq *struct {
		Usage *usage `json:"usage"`
		Error string `json:"error"`
	} `json:"x_groq"`
	Error *apiError `json:"error"`
}

func (a *Adapter) post(ctx context.Context, req provider.Request, stream bool) (*http.Response, error) {
	body := request{
		Model:               req.Model,
		Messages:            make([]message, len(req.Messages)),
		Temperature:         req.Temperature,
		TopP:                req.TopP,
		MaxCompletionTokens: req.MaxTokens,
		Stop:                req.Stop,
		Seed:                req.Seed,
	}
	for i, m := range req.Messages {
		body.Messages[i] = message(m)
	}
	if stream {
		body.Stream = true
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("provider %s: encode request: %w", a.name, err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("provider %s: build request: %w", a.name, err)
	}
	hreq.Header.Set("Authorization", "Bearer "+string(a.apiKey))
	hreq.Header.Set("Content-Type", "application/json")
	if stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}

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

	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if ctx.Err() != nil {
			return provider.Response{}, provider.TransportError(ctx, a.name, err)
		}
		return provider.Response{}, provider.MalformedError(a.name, err)
	}
	if len(out.Choices) == 0 {
		return provider.Response{}, provider.MalformedError(a.name, errors.New("response has no choices"))
	}
	r := provider.Response{Content: out.Choices[0].Message.Content, FinishReason: out.Choices[0].FinishReason, Model: out.Model}
	if out.Usage != nil {
		r.Usage = out.Usage.toProvider()
	}
	return r, nil
}

// Stream sends a streamed request and calls fn once per event. The usage is
// sent as the last event, once, whichever field the provider reported it in.
func (a *Adapter) Stream(ctx context.Context, req provider.Request, fn func(provider.Event) error) error {
	resp, err := a.post(ctx, req, true)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxEventSize)
	var final *usage
	var data []string
	// dispatch handles one complete SSE event and reports whether it was [DONE].
	dispatch := func() (bool, error) {
		payload := strings.Join(data, "\n")
		data = data[:0]
		if payload == "[DONE]" {
			if final == nil {
				return true, nil
			}
			u := final.toProvider()
			return true, fn(provider.Event{Usage: &u})
		}
		ev, u, err := a.parse(payload)
		if err != nil {
			return false, err
		}
		if u != nil {
			final = u
		}
		if ev.Delta != "" || ev.FinishReason != "" || ev.Progress {
			return false, fn(ev)
		}
		return false, nil
	}
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
			continue
		}
		if line != "" || len(data) == 0 {
			continue // a comment (": ..."), a field such as event:, or an empty event
		}
		if done, err := dispatch(); done || err != nil {
			return err
		}
	}
	if sc.Err() == nil && len(data) > 0 {
		// The body ended without the blank line that closes the last event.
		if done, err := dispatch(); done || err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return provider.MalformedError(a.name, fmt.Errorf("event larger than %d bytes", maxEventSize))
		}
		return provider.TransportError(ctx, a.name, err)
	}
	if ctx.Err() != nil {
		return provider.TransportError(ctx, a.name, ctx.Err())
	}
	return &provider.Error{Provider: a.name, StatusCode: resp.StatusCode, Kind: provider.KindUpstreamUnavailable, Err: errNoDone}
}

func (a *Adapter) parse(payload string) (provider.Event, *usage, error) {
	var c chunk
	if err := json.Unmarshal([]byte(payload), &c); err != nil {
		return provider.Event{}, nil, provider.MalformedError(a.name, err)
	}
	if c.Error != nil {
		return provider.Event{}, nil, &provider.Error{Provider: a.name, StatusCode: http.StatusOK, Kind: provider.KindUpstreamUnavailable, Err: errors.New(c.Error.Message)}
	}
	if c.XGroq != nil && c.XGroq.Error != "" {
		return provider.Event{}, nil, &provider.Error{Provider: a.name, StatusCode: http.StatusOK, Kind: provider.KindUpstreamUnavailable, Err: errors.New(c.XGroq.Error)}
	}
	ev := provider.Event{Model: c.Model}
	if len(c.Choices) > 0 {
		ev.Delta = c.Choices[0].Delta.Content
		ev.Progress = ev.Delta == "" && c.Choices[0].Delta.Reasoning != ""
		if fr := c.Choices[0].FinishReason; fr != nil {
			ev.FinishReason = *fr
		}
	}
	u := c.Usage
	if u == nil && c.XGroq != nil {
		u = c.XGroq.Usage
	}
	return ev, u, nil
}
