package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
)

// gateway is a running serve call on a loopback port, with a slow route added.
type gateway struct {
	url     string
	cancel  context.CancelFunc // sends the "signal"
	done    chan error         // serve's return value
	started chan struct{}      // receives once per /slow request that has begun
}

// startGateway adds GET /slow, which takes slowFor, or waits for release when slowFor is 0.
func startGateway(t *testing.T, slowFor time.Duration, release <-chan struct{}, drainDelay, shutdownTimeout time.Duration) *gateway {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	log := slog.New(slog.DiscardHandler)
	api := httpapi.New(log, httpapi.Deps{})
	g := &gateway{url: "http://" + ln.Addr().String(), done: make(chan error, 1), started: make(chan struct{}, 4)}

	mux := http.NewServeMux()
	mux.Handle("/", api)
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		g.started <- struct{}{}
		// Watching the request context catches a BaseContext derived from the signal.
		var finished <-chan time.Time
		if slowFor > 0 {
			finished = time.After(slowFor)
		}
		select {
		case <-finished:
		case <-release:
		case <-r.Context().Done():
			_, _ = io.WriteString(w, "request context ended early")
			return
		}
		_, _ = io.WriteString(w, "finished")
	})

	ctx, cancel := context.WithCancel(t.Context())
	g.cancel = cancel
	go func() {
		g.done <- serve(ctx, log, ln, httpapi.NewServer("", mux), api.StartDraining, drainDelay, shutdownTimeout)
	}()
	t.Cleanup(cancel)
	return g
}

func (g *gateway) get(t *testing.T, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.url+path, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// wait returns serve's error, or fails the test if serve does not return in time.
func (g *gateway) wait(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case err := <-g.done:
		return err
	case <-time.After(within):
		t.Fatalf("serve did not return within %v", within)
		return nil
	}
}

func TestInFlightRequestFinishesDuringShutdown(t *testing.T) {
	t.Parallel()
	g := startGateway(t, 300*time.Millisecond, nil, 0, 5*time.Second)

	type result struct {
		status int
		body   string
		err    error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := g.get(t, "/slow")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		got <- result{status: resp.StatusCode, body: string(b)}
	}()

	<-g.started // the request is in flight
	g.cancel()  // SIGTERM

	r := <-got
	if r.err != nil || r.status != http.StatusOK || r.body != "finished" {
		t.Errorf("in-flight request = %d %q (err %v), want 200 \"finished\"", r.status, r.body, r.err)
	}
	if err := g.wait(t, 5*time.Second); err != nil {
		t.Errorf("serve returned %v, want nil", err)
	}
}

func TestReadyzIs503DuringTheDrainDelay(t *testing.T) {
	t.Parallel()
	g := startGateway(t, 0, nil, 2*time.Second, 5*time.Second)

	resp, err := g.get(t, "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz before shutdown = %d, want 200", resp.StatusCode)
	}

	g.cancel()

	deadline := time.Now().Add(time.Second)
	for {
		resp, err := g.get(t, "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz during the drain: %v (the server must still accept connections)", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /readyz during the drain = %d, want 503", resp.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := g.wait(t, 5*time.Second); err != nil {
		t.Errorf("serve returned %v, want nil", err)
	}
}

func TestShutdownTimeoutReturnsADeadlineError(t *testing.T) {
	t.Parallel()
	release := make(chan struct{}) // never closed: the request outlives the timeout
	g := startGateway(t, 0, release, 0, 100*time.Millisecond)

	go func() {
		if resp, err := g.get(t, "/slow"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-g.started
	g.cancel()

	err := g.wait(t, 5*time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("serve returned %v, want a context.DeadlineExceeded error", err)
	}
}
