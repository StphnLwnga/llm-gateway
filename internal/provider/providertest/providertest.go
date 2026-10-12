// Package providertest is the contract suite every provider adapter must pass.
package providertest

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/provider"
)

// Wire writes an adapter's wire format, so the fake upstream can speak it.
type Wire interface {
	Delta(text string) []byte                                    // one stream chunk carrying text
	End(u provider.Usage, finish string) []byte                  // the chunks that close a stream
	Chat(content string, u provider.Usage, finish string) []byte // a complete non-streamed body
	Error(status int) []byte                                     // an error body
	Malformed() []byte                                           // a stream chunk that does not parse
}

// Adapter is the adapter under test.
type Adapter struct {
	Name string // the provider name its errors must carry
	New  func(t *testing.T, baseURL string, client *http.Client) provider.Provider
	Wire Wire
}

const (
	// hold bounds how long the fake upstream waits for a client to go away, so
	// an adapter that ignores ctx fails the test instead of hanging it.
	hold = 5 * time.Second
	// prompt is how soon a stopped call must return; far below hold.
	prompt = 2 * time.Second

	firstByte = 100 * time.Millisecond
)

var (
	request = provider.Request{Model: "test-model", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	usage   = provider.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8}
	deltas  = []string{"Hel", "lo", " world"}
)

// Run runs every contract case against a.
func Run(t *testing.T, a Adapter) {
	t.Helper()

	t.Run("stream delivers deltas in order", func(t *testing.T) {
		t.Parallel()
		u := start(t, a, firstByte, func(w http.ResponseWriter, _ *http.Request) {
			writeStream(w, a.Wire, deltas...)
			_, _ = w.Write(a.Wire.End(usage, "stop"))
		})
		events, err := collect(t.Context(), u.p)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		var got []string
		for _, e := range events {
			if e.Delta != "" {
				got = append(got, e.Delta)
			}
		}
		if !slices.Equal(got, deltas) {
			t.Errorf("deltas = %q, want %q", got, deltas)
		}
		u.bodiesClosed(t, 1)
	})

	t.Run("stream reports the final usage and finish reason", func(t *testing.T) {
		t.Parallel()
		u := start(t, a, firstByte, func(w http.ResponseWriter, _ *http.Request) {
			writeStream(w, a.Wire, deltas...)
			_, _ = w.Write(a.Wire.End(usage, "stop"))
		})
		events, err := collect(t.Context(), u.p)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		var usages []counts
		var finishes []string
		ended := false
		for _, e := range events {
			if e.Delta != "" && ended {
				t.Errorf("delta %q arrived after the finish reason or usage", e.Delta)
			}
			if e.Usage != nil {
				usages = append(usages, countsOf(*e.Usage))
				ended = true
			}
			if e.FinishReason != "" {
				finishes = append(finishes, e.FinishReason)
				ended = true
			}
		}
		if !slices.Equal(usages, []counts{countsOf(usage)}) {
			t.Errorf("usage events = %+v, want exactly one %+v", usages, usage)
		}
		if !slices.Equal(finishes, []string{"stop"}) {
			t.Errorf("finish reasons = %q, want exactly one \"stop\"", finishes)
		}
		u.bodiesClosed(t, 1)
	})

	t.Run("chat returns content, usage and finish reason", func(t *testing.T) {
		t.Parallel()
		u := start(t, a, firstByte, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(a.Wire.Chat("Hello world", usage, "length"))
		})
		resp, err := u.p.Chat(t.Context(), request)
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if resp.Content != "Hello world" || resp.FinishReason != "length" || countsOf(resp.Usage) != countsOf(usage) {
			t.Errorf("Chat = %q, %q, %+v; want %q, %q, %+v", resp.Content, resp.FinishReason, countsOf(resp.Usage), "Hello world", "length", countsOf(usage))
		}
		u.bodiesClosed(t, 1)
	})

	t.Run("stream cancelled mid-stream returns context.Canceled promptly", func(t *testing.T) {
		t.Parallel()
		u := start(t, a, firstByte, func(w http.ResponseWriter, r *http.Request) {
			writeStream(w, a.Wire, "one")
			waitForClient(r)
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		began := time.Now()
		err := u.p.Stream(ctx, request, func(provider.Event) error {
			calls++
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if calls != 1 {
			t.Errorf("callback ran %d times, want 1", calls)
		}
		returnedPromptly(t, began)
		u.bodiesClosed(t, 1)
	})

	for _, m := range methods {
		t.Run(m.name+" cancelled before the first byte returns context.Canceled promptly", func(t *testing.T) {
			t.Parallel()
			reached := make(chan struct{})
			u := start(t, a, hold, func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(reached)
				waitForClient(r)
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() {
				<-reached
				cancel()
			}()
			began := time.Now()
			if err := m.call(ctx, u.p); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			returnedPromptly(t, began)
			u.bodiesClosed(t, 0)
		})
	}

	t.Run("stream returns the callback error unchanged and stops reading", func(t *testing.T) {
		t.Parallel()
		u := start(t, a, firstByte, func(w http.ResponseWriter, r *http.Request) {
			writeStream(w, a.Wire, "one")
			waitForClient(r)
			writeStream(w, a.Wire, "two")
			_, _ = w.Write(a.Wire.End(usage, "stop"))
		})
		errStop := errors.New("client went away")
		calls := 0
		began := time.Now()
		err := u.p.Stream(t.Context(), request, func(provider.Event) error {
			calls++
			return errStop
		})
		if !errors.Is(err, errStop) {
			t.Fatalf("err = %v, want the callback's error", err)
		}
		if calls != 1 {
			t.Errorf("callback ran %d times, want 1", calls)
		}
		returnedPromptly(t, began)
		u.bodiesClosed(t, 1)
	})

	for _, m := range methods {
		t.Run(m.name+" times out waiting for the first byte", func(t *testing.T) {
			t.Parallel()
			u := start(t, a, firstByte, func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				waitForClient(r)
			})
			began := time.Now()
			err := m.call(t.Context(), u.p)
			wantKind(t, err, a.Name, provider.KindTimeout)
			var netErr net.Error
			if !errors.As(err, &netErr) || !netErr.Timeout() {
				t.Errorf("err = %v, want it to wrap the transport timeout", err)
			}
			returnedPromptly(t, began)
			u.bodiesClosed(t, 0)
		})
	}

	statuses := []struct {
		status     int
		retryAfter string
		kind       provider.Kind
		wantRetry  time.Duration
	}{
		{http.StatusTooManyRequests, "7", provider.KindRateLimited, 7 * time.Second},
		{http.StatusUnauthorized, "", provider.KindAuth, 0},
		{http.StatusInternalServerError, "", provider.KindUpstreamUnavailable, 0},
	}
	for _, m := range methods {
		for _, s := range statuses {
			t.Run(m.name+" maps status "+http.StatusText(s.status), func(t *testing.T) {
				t.Parallel()
				u := start(t, a, firstByte, func(w http.ResponseWriter, _ *http.Request) {
					if s.retryAfter != "" {
						w.Header().Set("Retry-After", s.retryAfter)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(s.status)
					_, _ = w.Write(a.Wire.Error(s.status))
				})
				err := m.call(t.Context(), u.p)
				wantKind(t, err, a.Name, s.kind)
				var pe *provider.Error
				if errors.As(err, &pe) && (pe.StatusCode != s.status || pe.RetryAfter != s.wantRetry) {
					t.Errorf("StatusCode, RetryAfter = %d, %v; want %d, %v", pe.StatusCode, pe.RetryAfter, s.status, s.wantRetry)
				}
				u.bodiesClosed(t, 1)
			})
		}
	}

	t.Run("stream maps a malformed chunk", func(t *testing.T) {
		t.Parallel()
		u := start(t, a, firstByte, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(a.Wire.Malformed())
		})
		_, err := collect(t.Context(), u.p)
		wantKind(t, err, a.Name, provider.KindUpstreamUnavailable)
		u.bodiesClosed(t, 1)
	})

	t.Run("chat maps a malformed body", func(t *testing.T) {
		t.Parallel()
		u := start(t, a, firstByte, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"truncated`)
		})
		_, err := u.p.Chat(t.Context(), request)
		wantKind(t, err, a.Name, provider.KindUpstreamUnavailable)
		u.bodiesClosed(t, 1)
	})
}

// counts is the part of Usage every adapter must report; timings are optional.
type counts struct {
	prompt, completion, total int
	promptUnknown             bool
}

func countsOf(u provider.Usage) counts {
	return counts{u.PromptTokens, u.CompletionTokens, u.TotalTokens, u.PromptTokensUnknown}
}

var methods = []struct {
	name string
	call func(ctx context.Context, p provider.Provider) error
}{
	{"chat", func(ctx context.Context, p provider.Provider) error {
		_, err := p.Chat(ctx, request)
		return err
	}},
	{"stream", func(ctx context.Context, p provider.Provider) error {
		return p.Stream(ctx, request, func(provider.Event) error { return nil })
	}},
}

type upstream struct {
	p      provider.Provider
	bodies *bodyTracker
}

func start(t *testing.T, a Adapter, headerTimeout time.Duration, h http.HandlerFunc) upstream {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client := provider.NewHTTPClient(time.Second, headerTimeout)
	tracker := &bodyTracker{next: client.Transport}
	client.Transport = tracker
	return upstream{p: a.New(t, srv.URL, client), bodies: tracker}
}

// bodiesClosed fails unless the adapter opened want response bodies through
// the client it was given, and closed every one before returning.
func (u upstream) bodiesClosed(t *testing.T, want int) {
	t.Helper()
	opened, open := u.bodies.counts()
	if opened != want {
		t.Errorf("adapter opened %d upstream bodies through its client, want %d", opened, want)
	}
	if open != 0 {
		t.Errorf("%d of %d upstream bodies were still open when the call returned", open, opened)
	}
}

type bodyTracker struct {
	next   http.RoundTripper
	mu     sync.Mutex
	bodies []*trackedBody
}

func (b *bodyTracker) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := b.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	tb := &trackedBody{ReadCloser: resp.Body}
	b.mu.Lock()
	b.bodies = append(b.bodies, tb)
	b.mu.Unlock()
	resp.Body = tb
	return resp, nil
}

func (b *bodyTracker) counts() (opened, open int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, tb := range b.bodies {
		if !tb.closed.Load() {
			open++
		}
	}
	return len(b.bodies), open
}

type trackedBody struct {
	io.ReadCloser
	closed atomic.Bool
}

func (tb *trackedBody) Close() error {
	tb.closed.Store(true)
	return tb.ReadCloser.Close()
}

func collect(ctx context.Context, p provider.Provider) ([]provider.Event, error) {
	var events []provider.Event
	err := p.Stream(ctx, request, func(e provider.Event) error {
		events = append(events, e)
		return nil
	})
	return events, err
}

func writeStream(w http.ResponseWriter, wire Wire, texts ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, s := range texts {
		_, _ = w.Write(wire.Delta(s))
	}
	w.(http.Flusher).Flush()
}

// waitForClient returns when the client disconnects, or after hold. The server
// notices a disconnect only once the request body has been read.
func waitForClient(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	select {
	case <-r.Context().Done():
	case <-time.After(hold):
	}
}

func returnedPromptly(t *testing.T, began time.Time) {
	t.Helper()
	if d := time.Since(began); d > prompt {
		t.Errorf("returned after %v, want under %v", d.Round(time.Millisecond), prompt)
	}
}

func wantKind(t *testing.T, err error, name string, kind provider.Kind) {
	t.Helper()
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Errorf("err = %v, want a *provider.Error of kind %s", err, kind)
		return
	}
	if pe.Kind != kind || pe.Provider != name {
		t.Errorf("Kind, Provider = %s, %q; want %s, %q", pe.Kind, pe.Provider, kind, name)
	}
}
