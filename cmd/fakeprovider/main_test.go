package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestRunRejectsAnInvalidPort(t *testing.T) {
	t.Parallel()
	for _, port := range []string{"", "0", "70000", "http"} {
		err := run(t.Context(), env(map[string]string{"PORT": port}), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "PORT") {
			t.Errorf("PORT=%q: err = %v, want an error naming PORT", port, err)
		}
	}
}

// syncBuffer is a bytes.Buffer that run's logger and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRunServesAndShutsDownOnCancel(t *testing.T) {
	t.Parallel()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	var logs syncBuffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, env(map[string]string{"PORT": port}), &logs) }()

	var resp *http.Response
	for range 50 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:"+port+"/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp == nil {
		t.Fatal("fakeprovider never answered GET /v1/models")
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/models = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	for _, msg := range []string{`"msg":"listening"`, `"msg":"shutdown complete"`} {
		if !strings.Contains(logs.String(), msg) {
			t.Errorf("logs miss %s:\n%s", msg, logs.String())
		}
	}
}
