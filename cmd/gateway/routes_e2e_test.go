package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/store/postgres/pgtest"
)

// syncBuffer lets run's logger and the test share one buffer.
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

// buildGatewayctl builds the real admin binary, so the test drives it the way an operator does.
func buildGatewayctl(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gatewayctl")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "github.com/StphnLwnga/llm-gateway/cmd/gatewayctl")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build gatewayctl: %v\n%s", err, out)
	}
	return bin
}

func TestRoutesAddedWithGatewayctlAppearInModels(t *testing.T) {
	t.Parallel()
	_, dsn := pgtest.Start(t)
	ctl := buildGatewayctl(t)
	gatewayctl := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), ctl, args...)
		cmd.Env = append(os.Environ(), "DATABASE_URL="+string(dsn), "LOG_FORMAT=text")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("gatewayctl %v: %v\n%s", args, err, out)
		}
	}

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	ctx, cancel := context.WithCancel(t.Context())
	var logs syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, env(map[string]string{
			"PORT": port, "DATABASE_URL": string(dsn), "ROUTES_REFRESH_INTERVAL": "200ms",
			"OLLAMA_BASE_URL": "http://127.0.0.1:1", // enabled; no request reaches it in this test
		}), &logs)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	get := func(path string) (int, []byte) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatalf("timed out waiting for %s\nlogs:\n%s", what, logs.String())
	}
	// aliases returns the listed model IDs, and false when the request failed.
	aliases := func() ([]string, bool) {
		code, body := get("/v1/models")
		if code != http.StatusOK {
			return nil, false
		}
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &list)
		out := []string{}
		for _, m := range list.Data {
			out = append(out, m.ID)
		}
		return out, true
	}

	// The routes table does not exist yet, so no snapshot has loaded.
	waitFor("the gateway to listen", func() bool { code, _ := get("/healthz"); return code == http.StatusOK })
	if code, _ := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before the routes loaded = %d, want 503", code)
	}

	gatewayctl("migrate", "up")
	waitFor("/readyz to be 200 after the first load", func() bool { code, _ := get("/readyz"); return code == http.StatusOK })

	gatewayctl("routes", "add", "--alias", "local-chat", "--provider", "ollama", "--upstream-model", "llama3.2:3b", "--priority", "1")
	gatewayctl("routes", "add", "--alias", "gpt", "--provider", "openai", "--upstream-model", "gpt-x", "--priority", "1")
	waitFor("local-chat in /v1/models", func() bool { a, ok := aliases(); return ok && slices.Equal(a, []string{"local-chat"}) })

	gatewayctl("routes", "disable", "1")
	waitFor("local-chat to leave /v1/models", func() bool { a, ok := aliases(); return ok && len(a) == 0 })
}
