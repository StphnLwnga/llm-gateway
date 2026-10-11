package sse_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/sse"
)

// serve runs h on a plain http.Server; its Close does not wait for handlers.
func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

func get(ctx context.Context, t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestEventsCommentsAndDoneAreFramedAndFlushed(t *testing.T) {
	t.Parallel()
	url := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		s := sse.NewWriter(w, time.Second)
		_ = s.Event(map[string]string{"text": "line one\nline two"})
		_ = s.Comment("heartbeat")
		time.Sleep(1500 * time.Millisecond) // longer than the write timeout: the deadline must be cleared
		_ = s.Done()
	})
	resp := get(t.Context(), t, url)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v (the response must end cleanly after an idle gap)", err)
	}
	want := `data: {"text":"line one\nline two"}` + "\n\n: heartbeat\n\ndata: [DONE]\n\n"
	if string(b) != want {
		t.Errorf("body:\n%q\nwant:\n%q", b, want)
	}
}

func TestAClientThatStopsReadingGivesErrClientGone(t *testing.T) {
	t.Parallel()
	result := make(chan error, 1)
	url := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		s := sse.NewWriter(w, 100*time.Millisecond)
		big := strings.Repeat("x", 64<<10)
		for {
			if err := s.Event(big); err != nil {
				result <- err
				return
			}
		}
	})
	resp := get(t.Context(), t, url) // headers only; the body is never read
	defer func() { _ = resp.Body.Close() }()
	select {
	case err := <-result:
		if !errors.Is(err, sse.ErrClientGone) {
			t.Errorf("err = %v, want ErrClientGone", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never gave up on a client that stopped reading")
	}
}

func TestAClientThatLeavesGivesErrClientGone(t *testing.T) {
	t.Parallel()
	result := make(chan error, 1)
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		s := sse.NewWriter(w, time.Second)
		_ = s.Comment("first")
		<-r.Context().Done()
		for range 50 { // the first writes after a disconnect can still land in buffers
			if err := s.Comment("after"); err != nil {
				result <- err
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		result <- nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	resp := get(ctx, t, url)
	cancel()
	_ = resp.Body.Close()
	if err := <-result; !errors.Is(err, sse.ErrClientGone) {
		t.Errorf("err = %v, want ErrClientGone", err)
	}
}
