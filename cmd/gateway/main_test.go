package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// env returns a lookup backed by a map. It stands in for the process environment.
func env(vars map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestRunFailsOnInvalidConfig(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	err := run(t.Context(), env(map[string]string{"PORT": "not-a-port"}), &stdout)
	if err == nil {
		t.Fatal("run succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "PORT") {
		t.Errorf("error %q does not name PORT", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("run logged before the config was valid: %s", stdout.String())
	}
}

// freePort returns a loopback port that was free a moment ago.
func freePort(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}

func TestRunLogsTheLoadedConfigAndStopsOnCancel(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already "signalled": run starts, then shuts down at once

	var stdout bytes.Buffer
	// Nothing listens on port 1: the pool is lazy, so run must still start and stop.
	vars := map[string]string{"PORT": port, "LOG_LEVEL": "debug", "DATABASE_URL": "postgres://gw:pw@127.0.0.1:1/gateway"}
	err := run(ctx, env(vars), &stdout)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	first, _, _ := bytes.Cut(stdout.Bytes(), []byte("\n"))
	var rec struct {
		Msg       string  `json:"msg"`
		Port      float64 `json:"port"`
		LogLevel  string  `json:"log_level"`
		LogFormat string  `json:"log_format"`
	}
	if err := json.Unmarshal(first, &rec); err != nil {
		t.Fatalf("first line is not JSON: %v\n%s", err, stdout.String())
	}
	wantPort, _ := strconv.Atoi(port)
	if rec.Msg != "config loaded" || int(rec.Port) != wantPort || rec.LogLevel != "DEBUG" || rec.LogFormat != "json" {
		t.Errorf("record = %+v, want config loaded, port %d, DEBUG, json", rec, wantPort)
	}
	for _, want := range []string{`"msg":"listening"`, `"msg":"shutdown complete"`} {
		if !bytes.Contains(stdout.Bytes(), []byte(want)) {
			t.Errorf("output is missing %s:\n%s", want, stdout.String())
		}
	}
}

func TestRunEnablesProvidersWithKeysAndNeverLogsTheKeys(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var stdout bytes.Buffer
	const groqKey = "gsk_live_do_not_log_0123456789"
	vars := map[string]string{
		"PORT": freePort(t), "DATABASE_URL": "postgres://gw:pw@127.0.0.1:1/gateway",
		"GROQ_API_KEY": groqKey, "OLLAMA_BASE_URL": "http://localhost:11434",
	}
	if err := run(ctx, env(vars), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := stdout.String()
	if strings.Contains(out, groqKey) {
		t.Fatalf("the logs contain the Groq key:\n%s", out)
	}
	for _, want := range []string{
		`"msg":"provider enabled","provider":"groq"`,
		`"msg":"provider disabled","provider":"openai","reason":"OPENAI_API_KEY is not set"`,
		`"msg":"provider enabled","provider":"ollama","base_url":"http://localhost:11434"`,
		`"msg":"providers ready","names":["groq","ollama"]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs miss %s:\n%s", want, out)
		}
	}
}

func TestRunReturnsTheListenErrorWithoutASignal(t *testing.T) {
	t.Parallel()
	var lc net.ListenConfig
	taken, err := lc.Listen(t.Context(), "tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	port := strconv.Itoa(taken.Addr().(*net.TCPAddr).Port)

	vars := map[string]string{"PORT": port, "DATABASE_URL": "postgres://gw:pw@127.0.0.1:1/gateway"}
	done := make(chan error, 1)
	go func() { done <- run(t.Context(), env(vars), io.Discard) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "listen") {
			t.Errorf("run = %v, want the listen error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5 s of the listen error")
	}
}
