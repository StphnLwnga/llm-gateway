package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestKindOf(t *testing.T) {
	t.Parallel()
	cases := map[int]Kind{
		400: KindBadRequest,
		401: KindAuth,
		403: KindAuth,
		404: KindBadRequest,
		408: KindTimeout,
		422: KindBadRequest,
		429: KindRateLimited,
		500: KindUpstreamUnavailable,
		502: KindUpstreamUnavailable,
		503: KindUpstreamUnavailable,
		504: KindTimeout,
		529: KindUpstreamUnavailable,
	}
	for status, want := range cases {
		if got := kindOf(status); got != want {
			t.Errorf("kindOf(%d) = %s, want %s", status, got, want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"7":                             7 * time.Second,
		"0":                             0,
		"-3":                            0,
		"soon":                          0,
		"Sun, 11 Oct 2026 12:00:30 GMT": 30 * time.Second,
		"Sun, 11 Oct 2026 11:59:00 GMT": 0, // already past
	}
	for v, want := range cases {
		if got := retryAfter(v, now); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestResponseErrorReadsStatusAndRetryAfter(t *testing.T) {
	t.Parallel()
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"12"}}}
	err := fmt.Errorf("route: %w", ResponseError("groq", resp))

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("errors.As found no *Error in %v", err)
	}
	if pe.Provider != "groq" || pe.StatusCode != 429 || pe.Kind != KindRateLimited || pe.RetryAfter != 12*time.Second {
		t.Errorf("got %+v", *pe)
	}
	if want := "route: provider groq: rate_limited (status 429)"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestTransportError(t *testing.T) {
	t.Parallel()

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		err := TransportError(t.Context(), "ollama", os.ErrDeadlineExceeded)
		var pe *Error
		if !errors.As(err, &pe) || pe.Kind != KindTimeout {
			t.Fatalf("err = %v, want kind timeout", err)
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("err = %v, want it to wrap the cause", err)
		}
	})

	t.Run("connection error", func(t *testing.T) {
		t.Parallel()
		err := TransportError(t.Context(), "ollama", io.ErrUnexpectedEOF)
		var pe *Error
		if !errors.As(err, &pe) || pe.Kind != KindUpstreamUnavailable || pe.StatusCode != 0 {
			t.Fatalf("err = %v, want kind upstream_unavailable with no status", err)
		}
	})

	t.Run("caller cancelled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := TransportError(ctx, "ollama", io.ErrUnexpectedEOF)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		var pe *Error
		if errors.As(err, &pe) {
			t.Errorf("a cancelled caller was reported as an upstream failure: %v", err)
		}
	})
}
