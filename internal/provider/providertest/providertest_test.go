package providertest_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/provider/providertest"
)

// reference is the smallest adapter that keeps the contract. It proves the
// suite: breaking any contract rule in it must turn the suite red.
type reference struct {
	baseURL string
	client  *http.Client
}

const refName = "reference"

type refUsage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

func (u refUsage) usage() provider.Usage {
	return provider.Usage{PromptTokens: u.Prompt, CompletionTokens: u.Completion, TotalTokens: u.Total}
}

type refChunk struct {
	Delta   string    `json:"delta,omitempty"`
	Content string    `json:"content,omitempty"`
	Finish  string    `json:"finish,omitempty"`
	Usage   *refUsage `json:"usage,omitempty"`
}

func (r reference) post(ctx context.Context, req provider.Request, stream bool) (*http.Response, error) {
	body, err := json.Marshal(map[string]any{"model": req.Model, "stream": stream})
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(hreq)
	if err != nil {
		return nil, provider.TransportError(ctx, refName, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, provider.ResponseError(refName, resp)
	}
	return resp, nil
}

func (r reference) Chat(ctx context.Context, req provider.Request) (provider.Response, error) {
	resp, err := r.post(ctx, req, false)
	if err != nil {
		return provider.Response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var c refChunk
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return provider.Response{}, provider.MalformedError(refName, err)
	}
	out := provider.Response{Content: c.Content, FinishReason: c.Finish}
	if c.Usage != nil {
		out.Usage = c.Usage.usage()
	}
	return out, nil
}

func (r reference) Stream(ctx context.Context, req provider.Request, fn func(provider.Event) error) error {
	resp, err := r.post(ctx, req, true)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var c refChunk
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return provider.MalformedError(refName, err)
		}
		ev := provider.Event{Delta: c.Delta, FinishReason: c.Finish}
		if c.Usage != nil {
			u := c.Usage.usage()
			ev.Usage = &u
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return provider.TransportError(ctx, refName, err)
	}
	return nil
}

// refWire is the reference adapter's wire format: one JSON object per line.
type refWire struct{}

func line(c refChunk) []byte {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

func (refWire) Delta(text string) []byte { return line(refChunk{Delta: text}) }

func (refWire) End(u provider.Usage, finish string) []byte {
	return line(refChunk{Finish: finish, Usage: &refUsage{u.PromptTokens, u.CompletionTokens, u.TotalTokens}})
}

func (refWire) Chat(content string, u provider.Usage, finish string) []byte {
	return line(refChunk{Content: content, Finish: finish, Usage: &refUsage{u.PromptTokens, u.CompletionTokens, u.TotalTokens}})
}

func (refWire) Error(status int) []byte {
	return []byte(fmt.Sprintf(`{"error":%q}`, http.StatusText(status)))
}

func (refWire) Malformed() []byte { return []byte("{\"delta\":\n") }

func TestReferenceAdapterKeepsTheContract(t *testing.T) {
	providertest.Run(t, providertest.Adapter{
		Name: refName,
		New: func(_ *testing.T, baseURL string, client *http.Client) provider.Provider {
			return reference{baseURL: baseURL, client: client}
		},
		Wire: refWire{},
	})
}
