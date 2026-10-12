package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Kind says what went wrong upstream, so a caller can decide whether to retry,
// fail over or give up without parsing provider-specific messages.
type Kind string

// The kinds of upstream error.
const (
	KindRateLimited         Kind = "rate_limited"
	KindAuth                Kind = "auth"
	KindBadRequest          Kind = "bad_request"
	KindUpstreamUnavailable Kind = "upstream_unavailable"
	KindTimeout             Kind = "timeout"
)

// Error is an upstream failure. Find it in a chain with errors.As.
type Error struct {
	Provider   string
	StatusCode int // 0 when no response arrived
	Kind       Kind
	RetryAfter time.Duration // 0 when the provider sent none
	Err        error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("provider %s: %s", e.Provider, e.Kind)
	if e.StatusCode != 0 {
		msg += fmt.Sprintf(" (status %d)", e.StatusCode)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error {
	return e.Err
}

// ResponseError classifies a non-2xx upstream response. It does not read or
// close the body.
func ResponseError(name string, resp *http.Response) *Error {
	return &Error{
		Provider:   name,
		StatusCode: resp.StatusCode,
		Kind:       kindOf(resp.StatusCode),
		RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}
}

// TransportError classifies an error from sending the request or reading the
// body. When ctx has ended, the caller gave up, so the error wraps ctx.Err()
// and is not an upstream failure.
func TransportError(ctx context.Context, name string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("provider %s: %w", name, ctxErr)
	}
	kind := KindUpstreamUnavailable
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		kind = KindTimeout
	}
	return &Error{Provider: name, Kind: kind, Err: err}
}

// MalformedError reports a response body that does not parse.
func MalformedError(name string, err error) *Error {
	return &Error{Provider: name, StatusCode: http.StatusOK, Kind: KindUpstreamUnavailable, Err: err}
}

func kindOf(status int) Kind {
	switch {
	case status == http.StatusTooManyRequests:
		return KindRateLimited
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return KindAuth
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return KindTimeout
	case status >= 400 && status < 500:
		return KindBadRequest
	default:
		return KindUpstreamUnavailable
	}
}

// retryAfter parses either form RFC 9110 allows: delay seconds or an HTTP date.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
