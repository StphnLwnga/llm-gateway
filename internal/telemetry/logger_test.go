package telemetry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestLoggerDropsRecordsBelowItsLevel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelWarn, "json")

	log.Debug("debug line")
	log.Info("info line")
	log.Warn("warn line")
	log.Error("error line")

	out := buf.String()
	for _, dropped := range []string{"debug line", "info line"} {
		if strings.Contains(out, dropped) {
			t.Errorf("output contains %q below the warn level:\n%s", dropped, out)
		}
	}
	for _, kept := range []string{"warn line", "error line"} {
		if !strings.Contains(out, kept) {
			t.Errorf("output is missing %q:\n%s", kept, out)
		}
	}
}

func TestJSONLoggerWritesOneObjectPerLine(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo, "json")

	log.Info("first", "port", 8080)
	log.Info("second")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), buf.String())
	}
	var rec struct {
		Level string  `json:"level"`
		Msg   string  `json:"msg"`
		Port  float64 `json:"port"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("line is not JSON: %v\n%s", err, lines[0])
	}
	if rec.Level != "INFO" || rec.Msg != "first" || rec.Port != 8080 {
		t.Errorf("record = %+v, want level INFO, msg first, port 8080", rec)
	}
}

func TestTextLoggerWritesKeyValuePairs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo, "text")

	log.Info("hello", "port", 8080)

	out := buf.String()
	if json.Valid(bytes.TrimSpace(buf.Bytes())) {
		t.Errorf("text format wrote JSON: %s", out)
	}
	for _, want := range []string{"level=INFO", "msg=hello", "port=8080"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing %q", out, want)
		}
	}
}
