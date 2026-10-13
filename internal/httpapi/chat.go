package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/sse"
)

// Target is one provider that can serve a model alias.
type Target struct {
	ProviderName     string
	Provider         provider.Provider
	UpstreamModel    string
	DefaultMaxTokens *int
	MaxMaxTokens     *int
}

// Resolver returns the targets for a model alias, in the order to try them.
type Resolver interface {
	Resolve(alias string) []Target
}

// ChatConfig holds the chat endpoint's limits.
type ChatConfig struct {
	StatusHold        time.Duration
	HeartbeatInterval time.Duration
	MaxBodyBytes      int64
}

// eventWriteTimeout bounds each event write. A client that is reading takes
// microseconds; only one that has stopped reading reaches it.
const eventWriteTimeout = 10 * time.Second

// chatLog collects the fields of the one log line each request writes.
type chatLog struct {
	requestID     string
	alias         string
	provider      string
	upstreamModel string
	stream        bool
	status        int
	committedBy   string // first_token, hold_elapsed or error
	ttft          time.Duration
	usage         *provider.Usage
	truncated     bool
	errKind       string
	errType       string // error.type of a cut stream
	clientGone    bool
	firstVisible  time.Duration // to the first content written to the client; streams only
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &chatLog{requestID: "req_" + randomHex(12)}
	w.Header().Set("X-Request-Id", rec.requestID)
	defer func() {
		h.recordChat(r.Context(), rec)
		h.logChat(r.Context(), rec, start)
	}()

	r.Body = http.MaxBytesReader(w, r.Body, h.chat.MaxBodyBytes)
	var in chatRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			rec.status = http.StatusRequestEntityTooLarge
			writeAPIError(w, rec.status, errorBody{Message: "The request body is too large.", Type: "invalid_request_error"})
			return
		}
		rec.status = http.StatusBadRequest
		writeAPIError(w, rec.status, errorBody{Message: "We could not parse the JSON body of your request.", Type: "invalid_request_error"})
		return
	}
	rec.alias, rec.stream = in.Model, in.Stream
	if err := in.validate(); err != nil {
		rec.status = writeInvalid(w, err)
		return
	}

	var targets []Target
	if h.resolver != nil {
		targets = h.resolver.Resolve(in.Model)
	}
	if len(targets) == 0 {
		rec.status = http.StatusNotFound
		writeAPIError(w, rec.status, errorBody{
			Message: "The model `" + in.Model + "` does not exist or you do not have access to it.",
			Type:    "invalid_request_error", Param: ptr("model"), Code: ptr("model_not_found"),
		})
		return
	}
	t := targets[0] // failover across targets comes in milestone 4
	rec.provider, rec.upstreamModel = t.ProviderName, t.UpstreamModel
	w.Header().Set("X-LLM-Gateway-Provider", t.ProviderName)
	preq, err := in.upstream(t)
	if err != nil {
		rec.status = writeInvalid(w, err)
		return
	}

	c := completion{id: "chatcmpl-" + randomHex(12), created: start.Unix(), model: in.Model}
	if in.Stream {
		includeUsage := in.StreamOptions != nil && in.StreamOptions.IncludeUsage
		h.stream(w, r, t, preq, c, includeUsage, rec, start)
		return
	}
	h.aggregate(w, r, t, preq, c, rec, start)
}

// stream holds the status until the first token, the first error or
// StatusHold, whichever comes first (ADR 0015).
func (h *Handler) stream(w http.ResponseWriter, r *http.Request, t Target, preq provider.Request,
	c completion, includeUsage bool, rec *chatLog, start time.Time) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	events := make(chan provider.Event)
	done := make(chan error, 1)
	go func() {
		done <- t.Provider.Stream(ctx, preq, func(e provider.Event) error {
			select {
			case events <- e:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	out := sse.NewWriter(w, eventWriteTimeout)
	hold := time.NewTimer(h.chat.StatusHold)
	defer hold.Stop()
	var heartbeat <-chan time.Time
	committed, sentRole := false, false
	commit := func(by string) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no") // stops proxies such as nginx from buffering the stream
		w.WriteHeader(http.StatusOK)
		committed, rec.status, rec.committedBy = true, http.StatusOK, by
	}
	// stop ends the stream early because the client is gone.
	stop := func() {
		rec.clientGone = true
		cancel()
		<-done
	}

	for {
		select {
		case e := <-events:
			if e.Delta == "" && e.FinishReason == "" && e.Usage == nil {
				continue // progress only; the heartbeat keeps a committed stream open
			}
			if e.Delta != "" && rec.ttft == 0 {
				rec.ttft = time.Since(start)
			}
			if !committed {
				commit("first_token")
			}
			heartbeat = nil
			if e.Usage != nil {
				rec.usage = e.Usage
			}
			if e.Delta == "" && e.FinishReason == "" {
				continue // usage only; it is sent after the finish reason
			}
			delta := map[string]any{"content": e.Delta}
			if !sentRole {
				delta["role"], sentRole = "assistant", true
			}
			if e.FinishReason != "" && e.Delta == "" {
				delete(delta, "content")
			}
			if err := out.Event(c.chunk(delta, e.FinishReason, includeUsage)); err != nil {
				stop()
				return
			}
			if e.Delta != "" && rec.firstVisible == 0 {
				rec.firstVisible = time.Since(start)
			}
		case err := <-done:
			if err != nil {
				if r.Context().Err() != nil {
					rec.clientGone = true
					return
				}
				h.streamError(r.Context(), w, out, err, committed, rec)
				return
			}
			if !committed {
				commit("first_token")
			}
			if includeUsage && rec.usage != nil {
				if err := out.Event(c.usageChunk(*rec.usage)); err != nil {
					rec.clientGone = true
					return
				}
			}
			if err := out.Done(); err != nil {
				rec.clientGone = true
			}
			return
		case <-hold.C:
			if !committed {
				commit("hold_elapsed")
				if err := out.Comment("heartbeat"); err != nil {
					stop()
					return
				}
				heartbeat = time.Tick(h.chat.HeartbeatInterval)
			}
		case <-heartbeat:
			if err := out.Comment("heartbeat"); err != nil {
				stop()
				return
			}
		case <-r.Context().Done():
			stop()
			return
		}
	}
}

// streamError reports an upstream error: as a real status before the commit,
// or as an error event after it. An error event is not followed by [DONE],
// so a client cannot mistake a cut answer for a complete one.
func (h *Handler) streamError(ctx context.Context, w http.ResponseWriter, out *sse.Writer, err error, committed bool, rec *chatLog) {
	status, body, retryAfter := upstreamError(err)
	rec.errKind = errKind(err)
	h.log.WarnContext(ctx, "upstream error", "request_id", rec.requestID, "provider", rec.provider, "err", err)
	if !committed {
		rec.status, rec.committedBy = status, "error"
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		writeAPIError(w, status, body)
		return
	}
	rec.truncated, rec.errType = true, provider.ErrorType(err)
	if werr := out.Event(errorEnvelope{Error: body}); werr != nil {
		rec.clientGone = true
	}
}

// aggregate serves a non-streamed request from an upstream stream, so the
// same watchdog applies and a long answer has no total time limit. The status
// is sent only at the end, so every error gets a real status.
func (h *Handler) aggregate(w http.ResponseWriter, r *http.Request, t Target, preq provider.Request,
	c completion, rec *chatLog, start time.Time) {
	var text strings.Builder
	finish := ""
	var usage provider.Usage
	err := t.Provider.Stream(r.Context(), preq, func(e provider.Event) error {
		if e.Delta != "" && rec.ttft == 0 {
			rec.ttft = time.Since(start)
		}
		text.WriteString(e.Delta)
		if e.FinishReason != "" {
			finish = e.FinishReason
		}
		if e.Usage != nil {
			usage = *e.Usage
			rec.usage = e.Usage
		}
		return nil
	})
	if err != nil {
		if r.Context().Err() != nil {
			rec.clientGone = true
			return
		}
		status, body, retryAfter := upstreamError(err)
		rec.status, rec.errKind = status, errKind(err)
		h.log.WarnContext(r.Context(), "upstream error", "request_id", rec.requestID, "provider", rec.provider, "err", err)
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		writeAPIError(w, status, body)
		return
	}
	rec.status = http.StatusOK
	writeJSON(w, http.StatusOK, c.response(text.String(), finish, usage))
}

func (h *Handler) logChat(ctx context.Context, rec *chatLog, start time.Time) {
	attrs := []any{
		"request_id", rec.requestID, "alias", rec.alias, "provider", rec.provider, "upstream_model", rec.upstreamModel,
		"stream", rec.stream, "status", rec.status, "duration_ms", time.Since(start).Milliseconds(), "truncated", rec.truncated,
	}
	if rec.committedBy != "" {
		attrs = append(attrs, "committed_by", rec.committedBy)
	}
	if rec.ttft > 0 {
		attrs = append(attrs, "ttft_ms", rec.ttft.Milliseconds())
	}
	if rec.usage != nil {
		attrs = append(attrs, "prompt_tokens", rec.usage.PromptTokens, "completion_tokens", rec.usage.CompletionTokens,
			"prompt_tokens_unknown", rec.usage.PromptTokensUnknown)
	}
	if rec.errKind != "" {
		attrs = append(attrs, "error_kind", rec.errKind)
	}
	if rec.clientGone {
		attrs = append(attrs, "client_gone", true)
	}
	h.log.InfoContext(ctx, "chat completion", attrs...)
}

// completion builds the OpenAI response bodies for one request.
type completion struct {
	id      string
	created int64
	model   string // the alias the client sent
}

func (c completion) chunk(delta map[string]any, finish string, includeUsage bool) map[string]any {
	var fr any
	if finish != "" {
		fr = finish
	}
	ch := map[string]any{
		"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "logprobs": nil, "finish_reason": fr}},
	}
	if includeUsage {
		ch["usage"] = nil
	}
	return ch
}

func (c completion) usageChunk(u provider.Usage) map[string]any {
	return map[string]any{
		"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
		"choices": []any{}, "usage": usageJSON(u),
	}
}

func (c completion) response(content, finish string, u provider.Usage) map[string]any {
	return map[string]any{
		"id": c.id, "object": "chat.completion", "created": c.created, "model": c.model,
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "logprobs": nil, "finish_reason": finish,
		}},
		"usage": usageJSON(u),
	}
}

func usageJSON(u provider.Usage) map[string]int {
	return map[string]int{"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens, "total_tokens": u.TotalTokens}
}

// upstreamError maps a provider error to the client's status and a fixed
// public message. The upstream's own text goes only to the log.
func upstreamError(err error) (status int, body errorBody, retryAfter string) {
	var pe *provider.Error
	if !errors.As(err, &pe) {
		return http.StatusInternalServerError, errorBody{Message: serverErrorMessage, Type: "server_error"}, ""
	}
	switch pe.Kind {
	case provider.KindRateLimited:
		if pe.RetryAfter > 0 {
			retryAfter = strconv.FormatInt(int64(math.Ceil(pe.RetryAfter.Seconds())), 10)
		}
		return http.StatusTooManyRequests, errorBody{Message: "The upstream provider is rate limiting requests. Retry later.",
			Type: "rate_limit_error", Code: ptr("rate_limit_exceeded")}, retryAfter
	case provider.KindAuth:
		return http.StatusBadGateway, errorBody{Message: "The gateway could not authenticate with the upstream provider.",
			Type: "server_error", Code: ptr("upstream_auth")}, ""
	case provider.KindBadRequest:
		return http.StatusBadRequest, errorBody{Message: "The upstream provider rejected the request.", Type: "invalid_request_error"}, ""
	case provider.KindTimeout:
		return http.StatusGatewayTimeout, errorBody{Message: "The upstream provider timed out.",
			Type: "server_error", Code: ptr("upstream_timeout")}, ""
	default:
		return http.StatusBadGateway, errorBody{Message: "The upstream provider failed.",
			Type: "server_error", Code: ptr("upstream_unavailable")}, ""
	}
}

func errKind(err error) string {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return string(pe.Kind)
	}
	return "internal"
}

func writeInvalid(w http.ResponseWriter, err error) int {
	var ir *invalidRequest
	if !errors.As(err, &ir) {
		writeAPIError(w, http.StatusInternalServerError, errorBody{Message: serverErrorMessage, Type: "server_error"})
		return http.StatusInternalServerError
	}
	writeAPIError(w, http.StatusBadRequest, errorBody{Message: ir.msg, Type: "invalid_request_error", Param: ptr(ir.param)})
	return http.StatusBadRequest
}

func writeAPIError(w http.ResponseWriter, status int, body errorBody) {
	writeJSON(w, status, errorEnvelope{Error: body})
}

func ptr[T any](v T) *T { return &v }

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}
