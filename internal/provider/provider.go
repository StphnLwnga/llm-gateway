// Package provider defines the contract every upstream model adapter implements.
package provider

import "context"

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
}

// Event is one step of a streamed answer: a text delta, the final usage, the
// finish reason, or a combination.
type Event struct {
	Delta        string
	Usage        *Usage
	FinishReason string
}

// Response is a complete, non-streamed answer.
type Response struct {
	Content      string
	Usage        Usage
	FinishReason string
}

// Provider is an upstream model API.
type Provider interface {
	Chat(ctx context.Context, req Request) (Response, error)

	// Stream calls fn once per event, in order. It closes the upstream body
	// before it returns, on every path. An error from fn stops the stream and
	// is returned unchanged; when ctx ends, the error wraps ctx.Err().
	Stream(ctx context.Context, req Request, fn func(Event) error) error
}
