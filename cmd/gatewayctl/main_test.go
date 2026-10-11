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
