package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/StphnLwnga/llm-gateway/internal/provider"
)

// chatRequest is the OpenAI chat completions request. Unknown fields are
// ignored, because SDKs send fields the gateway does not use.
type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	MaxTokens           *int            `json:"max_tokens"`
	Stop                stopList        `json:"stop"`
	Seed                *int64          `json:"seed"`
	N                   *int            `json:"n"`
	Tools               json.RawMessage `json:"tools"`
	ResponseFormat      json.RawMessage `json:"response_format"`
	Logprobs            *bool           `json:"logprobs"`
}

type chatMessage struct {
	Role    string      `json:"role"`
	Content textContent `json:"content"`
}

// textContent accepts a string or an array of text parts.
type textContent struct {
	text string
	err  error // a part type the gateway does not support
}

func (c *textContent) UnmarshalJSON(b []byte) error {
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte(`"`)) {
		return json.Unmarshal(b, &c.text)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return errors.New("content must be a string or an array of text parts")
	}
	for _, p := range parts {
		if p.Type != "text" {
			c.err = fmt.Errorf("content part type %q is not supported; only text parts are", p.Type)
			return nil
		}
		c.text += p.Text
	}
	return nil
}

// stopList accepts a string or an array of strings.
type stopList []string

func (s *stopList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("stop must be a string or an array of strings")
	}
	*s = many
	return nil
}

// invalidRequest is a 400 with the parameter it is about.
type invalidRequest struct {
	param string
	msg   string
}

func (e *invalidRequest) Error() string { return e.param + ": " + e.msg }

func invalid(param, format string, args ...any) error {
	return &invalidRequest{param: param, msg: fmt.Sprintf(format, args...)}
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// validate checks everything that does not depend on the route.
func (r *chatRequest) validate() error {
	switch {
	case r.Model == "":
		return invalid("model", "you must provide a model parameter")
	case len(r.Messages) == 0:
		return invalid("messages", "messages must contain at least one message")
	case present(r.Tools):
		return invalid("tools", "tools are not supported yet")
	case present(r.ResponseFormat):
		return invalid("response_format", "response_format is not supported yet")
	case r.Logprobs != nil && *r.Logprobs:
		return invalid("logprobs", "logprobs are not supported yet")
	case r.N != nil && *r.N != 1:
		return invalid("n", "only n=1 is supported")
	case r.MaxCompletionTokens != nil && r.MaxTokens != nil && *r.MaxCompletionTokens != *r.MaxTokens:
		return invalid("max_completion_tokens", "max_completion_tokens and max_tokens disagree; send one of them")
	}
	for i, m := range r.Messages {
		param := fmt.Sprintf("messages[%d]", i)
		switch m.Role {
		case "system", "developer", "user", "assistant":
		case "tool", "function":
			return invalid(param+".role", "role %q needs tools, which are not supported yet", m.Role)
		default:
			return invalid(param+".role", "unknown role %q", m.Role)
		}
		if m.Content.err != nil {
			return invalid(param+".content", "%s", m.Content.err)
		}
	}
	if t := r.maxTokens(); t != nil && *t < 1 {
		return invalid("max_completion_tokens", "must be 1 or more")
	}
	return nil
}

func (r *chatRequest) maxTokens() *int {
	if r.MaxCompletionTokens != nil {
		return r.MaxCompletionTokens
	}
	return r.MaxTokens
}

// upstream builds the provider request for one target, applying the route's
// token limits.
func (r *chatRequest) upstream(t Target) (provider.Request, error) {
	maxTokens := r.maxTokens()
	if maxTokens == nil {
		maxTokens = t.DefaultMaxTokens
	}
	if maxTokens != nil && t.MaxMaxTokens != nil && *maxTokens > *t.MaxMaxTokens {
		return provider.Request{}, invalid("max_completion_tokens", "must be %d or less for model %s", *t.MaxMaxTokens, r.Model)
	}
	req := provider.Request{
		Model: t.UpstreamModel, Temperature: r.Temperature, TopP: r.TopP, MaxTokens: maxTokens, Stop: r.Stop, Seed: r.Seed,
		Messages: make([]provider.Message, len(r.Messages)),
	}
	for i, m := range r.Messages {
		role := m.Role
		if role == "developer" {
			role = "system" // OpenAI's newer name; not every upstream accepts it
		}
		req.Messages[i] = provider.Message{Role: role, Content: m.Content.text}
	}
	return req, nil
}
