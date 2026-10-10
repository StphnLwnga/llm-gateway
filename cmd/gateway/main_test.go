package main

import (
	"bytes"
	"encoding/json"
	"io"
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

func TestRunLogsTheLoadedConfig(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	err := run(t.Context(), env(map[string]string{"PORT": "9090", "LOG_LEVEL": "debug"}), &stdout, io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var rec struct {
		Msg       string  `json:"msg"`
		Port      float64 `json:"port"`
		LogLevel  string  `json:"log_level"`
		LogFormat string  `json:"log_format"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rec); err != nil {
		t.Fatalf("output is not one JSON line: %v\n%s", err, stdout.String())
	}
	if rec.Msg != "config loaded" || rec.Port != 9090 || rec.LogLevel != "DEBUG" || rec.LogFormat != "json" {
		t.Errorf("record = %+v, want config loaded, port 9090, DEBUG, json", rec)
	}
}
