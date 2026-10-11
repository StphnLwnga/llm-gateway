// Package fakeupstream is an OpenAI-compatible chat completions server with
// deterministic output and per-request fault modes, for tests and load tests.
package fakeupstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const completionID = "chatcmpl-fake"

// words is the fixed answer text; token i is words[i%len(words)].
var words = []string{"The", " quick", " brown", " fox", " jumps", " over", " the", " lazy", " dog", "."}

// New returns the fake upstream's handler.
func New() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", chat)
	mux.HandleFunc("GET /v1/models", models)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, apiError{
			Message: fmt.Sprintf("Invalid URL (%s %s)", r.Method, r.URL.Path),
			Type:    "invalid_request_error",
		})
	})
	return mux
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream        bool `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens *int `json:"max_tokens"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type delta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type choice struct {
	Index        int      `json:"index"`
	Delta        *delta   `json:"delta,omitempty"`
	Message      *message `json:"message,omitempty"`
	FinishReason *string  `json:"finish_reason"`
}

type completion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   any      `json:"usage,omitempty"`
}

// jsonNull makes a field print as null instead of being left out, as OpenAI
// does for usage on every chunk when include_usage is set.
var jsonNull = json.RawMessage("null")

func chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, apiError{Message: "invalid JSON body: " + err.Error(), Type: "invalid_request_error"})
		return
	}
	// The server notices a client disconnect only once the body is fully read.
	_, _ = io.Copy(io.Discard, r.Body)

	p, err := parseModel(req.Model)
	if errors.Is(err, errUnknownModel) {
		writeError(w, http.StatusNotFound, apiError{
			Message: fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", req.Model),
			Type:    "invalid_request_error",
			Code:    ptr("model_not_found"),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, apiError{Message: err.Error(), Type: "invalid_request_error", Param: ptr("model")})
		return
	}

	ctx := r.Context()
	if !sleep(ctx, p.ttfb) {
		return
	}
	if p.status != 0 {
		if p.retryAfter >= 0 {
			w.Header().Set("Retry-After", strconv.Itoa(p.retryAfter))
		}
		writeError(w, p.status, statusError(p.status))
		return
	}

	n, finish := p.tokens, "stop"
	if req.MaxTokens != nil && *req.MaxTokens < n {
		n, finish = max(*req.MaxTokens, 0), "length"
	}
	prompt := 0
	for _, m := range req.Messages {
		prompt += len(strings.Fields(m.Content))
	}
	u := usage{PromptTokens: prompt, CompletionTokens: n, TotalTokens: prompt + n}

	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		stream(ctx, w, p, n, finish, u, includeUsage)
		return
	}
	complete(ctx, w, p, n, finish, u)
}

func stream(ctx context.Context, w http.ResponseWriter, p plan, n int, finish string, u usage, includeUsage bool) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	var chunkUsage any
	if includeUsage {
		chunkUsage = jsonNull
	}
	send := func(c choice) {
		writeEvent(w, completion{ID: completionID, Object: "chat.completion.chunk", Model: Model, Choices: []choice{c}, Usage: chunkUsage})
	}

	if !sleep(ctx, p.firstToken) {
		return
	}
	send(choice{Delta: &delta{Role: "assistant", Content: ptr("")}})
	for i := 0; ; i++ {
		switch i {
		case p.stallAfter:
			_ = rc.Flush()
			<-ctx.Done()
			return
		case p.dropAfter:
			_ = rc.Flush()
			drop(rc)
			return
		case p.malformedAfter:
			_, _ = io.WriteString(w, "data: {\"id\":\n\n")
			_ = rc.Flush()
			return
		}
		if i == n {
			break
		}
		if i > 0 && !sleep(ctx, p.tpot) {
			return
		}
		send(choice{Delta: &delta{Content: ptr(token(i))}})
		_ = rc.Flush()
	}

	send(choice{Delta: &delta{}, FinishReason: ptr(finish)})
	if includeUsage {
		writeEvent(w, completion{ID: completionID, Object: "chat.completion.chunk", Model: Model, Choices: []choice{}, Usage: u})
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	_ = rc.Flush()
}

func complete(ctx context.Context, w http.ResponseWriter, p plan, n int, finish string, u usage) {
	if !sleep(ctx, p.firstToken) {
		return
	}
	var text strings.Builder
	for i := 0; ; i++ {
		switch i {
		case p.stallAfter:
			<-ctx.Done()
			return
		case p.dropAfter:
			drop(http.NewResponseController(w))
			return
		case p.malformedAfter:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"`+completionID+`","object":`)
			return
		}
		if i == n {
			break
		}
		if i > 0 && !sleep(ctx, p.tpot) {
			return
		}
		text.WriteString(token(i))
	}
	writeJSON(w, http.StatusOK, completion{
		ID:      completionID,
		Object:  "chat.completion",
		Model:   Model,
		Choices: []choice{{Message: &message{Role: "assistant", Content: text.String()}, FinishReason: ptr(finish)}},
		Usage:   u,
	})
}

// drop closes the TCP connection with no clean end of the response, like an
// upstream that dies mid-stream.
func drop(rc *http.ResponseController) {
	conn, _, err := rc.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}

func models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   []map[string]any{{"id": Model, "object": "model", "created": 0, "owned_by": "llm-gateway"}},
	})
}

func token(i int) string {
	return words[i%len(words)]
}

// sleep waits d, or until ctx ends, so no delay outlives its request.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func ptr[T any](v T) *T {
	return &v
}

func writeEvent(w io.Writer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // every value written here is a fixed struct
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, e apiError) {
	writeJSON(w, status, map[string]apiError{"error": e})
}

func statusError(status int) apiError {
	switch {
	case status == http.StatusTooManyRequests:
		return apiError{Message: "Rate limit reached for requests", Type: "requests", Code: ptr("rate_limit_exceeded")}
	case status >= 500:
		return apiError{Message: "The server had an error while processing your request. Sorry about that!", Type: "server_error"}
	default:
		return apiError{Message: http.StatusText(status), Type: "invalid_request_error"}
	}
}
