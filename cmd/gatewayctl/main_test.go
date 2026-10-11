package main

import (
	"bytes"
	"errors"
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

func TestWrongArgumentsPrintUsageAndExit2(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		nil,
		{"migrate"},
		{"migrate", "sideways"},
		{"tenants", "list"},
		{"migrate", "up", "extra"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			err := run(t.Context(), args, env(nil), io.Discard, &stderr)
			if !errors.Is(err, errUsage) {
				t.Fatalf("run(%q) = %v, want errUsage", args, err)
			}
			if code := exitCode(err); code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "usage: gatewayctl migrate up|down|status") {
				t.Errorf("stderr does not show usage:\n%s", stderr.String())
			}
		})
	}
}

func TestMigrateNeedsDatabaseURL(t *testing.T) {
	t.Parallel()
	err := run(t.Context(), []string{"migrate", "status"}, env(nil), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("run without DATABASE_URL = %v, want an error naming DATABASE_URL", err)
	}
	if code := exitCode(err); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestExitCodeForSuccess(t *testing.T) {
	t.Parallel()
	if code := exitCode(nil); code != 0 {
		t.Errorf("exitCode(nil) = %d, want 0", code)
	}
}

func TestRoutesArgumentShapes(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"routes"},
		{"routes", "list", "extra"},
		{"routes", "enable"},
		{"routes", "enable", "seven"},
		{"routes", "disable", "0"},
		{"routes", "add", "--provider", "groq", "--upstream-model", "m", "--priority", "1"},
		{"routes", "add", "--alias", "chat", "--provider", "groq", "--upstream-model", "m"},
		{"routes", "add", "--alias", "chat", "--provider", "groq", "--upstream-model", "m", "--priority", "1", "stray"},
		{"routes", "add", "--nope"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			if err := run(t.Context(), args, env(nil), io.Discard, io.Discard); !errors.Is(err, errUsage) {
				t.Errorf("run(%q) = %v, want errUsage", args, err)
			}
		})
	}
}

func TestRoutesAddRejectsInvalidValuesBeforeTouchingTheDatabase(t *testing.T) {
	t.Parallel()
	base := []string{"routes", "add", "--alias", "chat", "--upstream-model", "m", "--priority", "1"}
	cases := map[string]struct {
		extra []string
		want  string
	}{
		"unknown provider":  {[]string{"--provider", "anthropic"}, "provider"},
		"negative price":    {[]string{"--provider", "groq", "--input-price", "-0.5"}, "--input-price"},
		"too many decimals": {[]string{"--provider", "groq", "--output-price", "0.0000001"}, "--output-price"},
		"max below default": {[]string{"--provider", "groq", "--default-max-tokens", "100", "--max-max-tokens", "10"}, "max max tokens"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// No DATABASE_URL: the error must come from validation, not from the config or the database.
			err := run(t.Context(), append(append([]string{}, base...), tc.extra...), env(nil), io.Discard, io.Discard)
			if err == nil || errors.Is(err, errUsage) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want a validation error naming %s", err, tc.want)
			}
		})
	}
}
