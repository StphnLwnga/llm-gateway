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
	err := run(t.Context(), env(map[string]string{"PORT": "not-a-port"}), &stdout, io.Discard)
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
	err := run(ctx, env(vars), &stdout, io.Discard)
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
