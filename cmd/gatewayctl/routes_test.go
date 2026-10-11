package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/StphnLwnga/llm-gateway/internal/store/postgres/pgtest"
)

func TestRoutesCommandsAgainstPostgres(t *testing.T) {
	t.Parallel()
	_, dsn := pgtest.Start(t)
	vars := env(map[string]string{"DATABASE_URL": string(dsn), "LOG_FORMAT": "text"})
	ctl := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := run(t.Context(), args, vars, &out, io.Discard)
		return out.String(), err
	}

	if _, err := ctl("migrate", "up"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	out, err := ctl("routes", "add", "--alias", "chat", "--provider", "groq", "--upstream-model", "llama-8b",
		"--priority", "10", "--input-price", "0.05", "--output-price", "0.08", "--default-max-tokens", "256", "--max-max-tokens", "4096")
	if err != nil || !strings.Contains(out, "route 1 added: chat -> groq/llama-8b, priority 10") {
		t.Fatalf("routes add = %q, %v", out, err)
	}
	if _, err := ctl("routes", "add", "--alias", "chat", "--provider", "ollama", "--upstream-model", "llama3.2:3b", "--priority", "20", "--disabled"); err != nil {
		t.Fatalf("routes add second: %v", err)
	}
	if _, err := ctl("routes", "add", "--alias", "chat", "--provider", "openai", "--upstream-model", "x", "--priority", "10"); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Errorf("a second route at the same priority = %v, want a conflict", err)
	}

	out, err = ctl("routes", "list")
	if err != nil {
		t.Fatalf("routes list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") {
		t.Fatalf("routes list printed:\n%s", out)
	}
	for _, want := range [][]string{
		{"1", "chat", "10", "groq", "llama-8b", "true", "0.050000", "0.080000", "256", "4096"},
		{"2", "chat", "20", "ollama", "llama3.2:3b", "false", "0.000000", "0.000000", "-", "-"},
	} {
		if got := strings.Fields(lines[1+indexOf(want[0], lines[1:])]); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("row = %q, want %q", got, want)
		}
	}

	if out, err := ctl("routes", "enable", "2"); err != nil || !strings.Contains(out, "route 2 enabled") {
		t.Errorf("routes enable 2 = %q, %v", out, err)
	}
	if out, err := ctl("routes", "disable", "1"); err != nil || !strings.Contains(out, "route 1 disabled") {
		t.Errorf("routes disable 1 = %q, %v", out, err)
	}
	if _, err := ctl("routes", "enable", "99"); err == nil || !strings.Contains(err.Error(), "route not found") {
		t.Errorf("routes enable 99 = %v, want route not found", err)
	}
}

func indexOf(id string, rows []string) int {
	for i, r := range rows {
		if f := strings.Fields(r); len(f) > 0 && f[0] == id {
			return i
		}
	}
	return 0
}
