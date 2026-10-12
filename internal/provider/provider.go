// Package provider defines the contract every upstream model adapter implements.
package provider

import (
	"context"
	"time"
)

// Message is one chat message.
type Message struct {
	Role    string
	Content string
}

// Request is a chat request after routing. A nil optional field is left out of
// the upstream request, because zero is a valid value for each of them.
type Request struct {
	Model       string // the upstream model name
	Messages    []Message
	Temperature *float64
	TopP        *float64
	MaxTokens   *int
	Stop        []string
	Seed        *int64
}

// Usage is the token count the provider reports for one request.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int

	// PromptTokensUnknown is set when the provider did not report the prompt
	// tokens. PromptTokens and TotalTokens are then 0 and must not be billed
	// as real counts.
	PromptTokensUnknown bool

	Timings *Timings // nil when the provider reports no timings
}

// Timings is the provider's own account of where the time went. A nil field
// was not reported, which is different from zero.
type Timings struct {
	Queue   *time.Duration
	Load    *time.Duration
	Prefill *time.Duration
	Decode  *time.Duration
	// Total is the provider's own total, which each provider bounds
	// differently: Groq's leaves out the queue, Ollama's includes the load.
	Total *time.Duration
}

// Event is one step of a streamed answer: a text delta, the final usage, the
// finish reason, or a combination.
type Event struct {
	Delta        string
	Usage        *Usage
	FinishReason string
	Model        string // the model the upstream says answered; empty when it does not say

	// Progress marks an event with no content: the model is working, for
	// example reasoning, so a liveness deadline can reset.
	Progress bool
}

// Response is a complete, non-streamed answer.
type Response struct {
	Content      string
	Usage        Usage
	FinishReason string
	Model        string // the model the upstream says answered; empty when it does not say
}

// Provider is an upstream model API.
type Provider interface {
	Chat(ctx context.Context, req Request) (Response, error)

	// Stream calls fn once per event, in order. It closes the upstream body
	// before it returns, on every path. An error from fn stops the stream and
	// is returned unchanged; when ctx ends, the error wraps ctx.Err().
	Stream(ctx context.Context, req Request, fn func(Event) error) error
}
