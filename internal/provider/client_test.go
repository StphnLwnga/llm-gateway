package provider

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestHTTPClientLetsAStreamOutliveTheFirstByteTimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range 4 {
			_, _ = io.WriteString(w, "chunk\n")
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)

	client := NewHTTPClient(time.Second, 50*time.Millisecond)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading a 400ms stream with a 50ms first-byte timeout: %v", err)
	}
	if len(body) != 4*len("chunk\n") {
		t.Errorf("read %d bytes, want %d", len(body), 4*len("chunk\n"))
	}
}

func TestHTTPClientTimesOutWaitingForHeaders(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	client := NewHTTPClient(time.Second, 50*time.Millisecond)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("Do succeeded, want a first-byte timeout")
	}
	var pe *Error
	if !errors.As(TransportError(t.Context(), "test", err), &pe) || pe.Kind != KindTimeout {
		t.Errorf("err = %v, want kind timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("timed out after %v, want about 50ms", elapsed)
	}
}

func TestHTTPClientReusesConnectionsUnderConcurrency(t *testing.T) {
	t.Parallel()
	clients := map[string]func() *http.Client{
		"plain": func() *http.Client { return NewHTTPClient(5*time.Second, 5*time.Second) },
		// The tracing transport wraps the pooled one; the pool must still decide.
		"instrumented": func() *http.Client {
			return Instrument(NewHTTPClient(5*time.Second, 5*time.Second), tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider(), propagation.TraceContext{})
		},
	}
	for name, newClient := range clients {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkConnectionReuse(t, newClient())
		})
	}
}

func checkConnectionReuse(t *testing.T, client *http.Client) {
	const workers, perWorker = 50, 10
	var dialed, arrived atomic.Int64
	allArrived := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The warm-up round holds every request until all are in flight, so it
		// opens exactly one connection per worker.
		if arrived.Add(1) == workers {
			close(allArrived)
		}
		<-allArrived
		time.Sleep(2 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			dialed.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	run := func(n int) {
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				for range n {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
					if err != nil {
						t.Error(err)
						return
					}
					resp, err := client.Do(req)
					if err != nil {
						t.Error(err)
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
			})
		}
		wg.Wait()
	}

	run(1)
	warm := dialed.Load()
	run(perWorker)

	// Every worker holds at most one connection, and at least one per worker is
	// idle after the warm-up, so a kept pool never needs to dial again.
	if n := dialed.Load() - warm; n > 0 {
		t.Errorf("%d workers x %d requests after warm-up opened %d new connections, want 0", workers, perWorker, n)
	}
}
