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
	var dialed atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Millisecond) // keeps all workers in flight at once
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			dialed.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	const workers, perWorker = 50, 10
	client := NewHTTPClient(time.Second, time.Second)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range perWorker {
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

	if n := dialed.Load(); n > workers+5 {
		t.Errorf("%d workers x %d requests opened %d connections, want at most %d", workers, perWorker, n, workers+5)
	}
}
