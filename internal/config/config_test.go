package config

import (
	"fmt"
	"log/slog"
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

func TestLoadAppliesDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr() != ":8080" {
		t.Errorf("Addr() = %q, want %q", cfg.Addr(), ":8080")
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
	if cfg.LogFormat != FormatJSON {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, FormatJSON)
	}
}

func TestLoadParsesValidSettings(t *testing.T) {
	t.Parallel()
	cfg, err := Load(env(map[string]string{"PORT": "9090", "LOG_LEVEL": "WARN", "LOG_FORMAT": "text"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelWarn)
	}
	if cfg.LogFormat != FormatText {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, FormatText)
	}
}

func TestLoadRejectsInvalidSettings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		vars    map[string]string
		wantKey string // the setting the error must name
	}{
		{"port not a number", map[string]string{"PORT": "80a0"}, "PORT"},
		{"port too large", map[string]string{"PORT": "70000"}, "PORT"},
		{"port zero", map[string]string{"PORT": "0"}, "PORT"},
		{"port set but empty", map[string]string{"PORT": ""}, "PORT"},
		{"unknown log level", map[string]string{"LOG_LEVEL": "verbose"}, "LOG_LEVEL"},
		{"log level with offset", map[string]string{"LOG_LEVEL": "info+2"}, "LOG_LEVEL"},
		{"log level set but empty", map[string]string{"LOG_LEVEL": ""}, "LOG_LEVEL"},
		{"unknown log format", map[string]string{"LOG_FORMAT": "yaml"}, "LOG_FORMAT"},
		{"log format set but empty", map[string]string{"LOG_FORMAT": ""}, "LOG_FORMAT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(env(tc.vars))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("error %q does not name %s", err, tc.wantKey)
			}
		})
	}
}

func TestLoadReportsEveryInvalidSetting(t *testing.T) {
	t.Parallel()
	_, err := Load(env(map[string]string{"PORT": "x", "LOG_LEVEL": "loud", "LOG_FORMAT": "xml"}))
	if err == nil {
		t.Fatal("Load succeeded, want an error")
	}
	for _, key := range []string{"PORT", "LOG_LEVEL", "LOG_FORMAT"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not name %s", err, key)
		}
	}
}

var secretVerbs = []string{"%s", "%v", "%+v", "%#v", "%d", "%x", "%q"}

func TestEmptySecretPrintsNotSet(t *testing.T) {
	t.Parallel()
	var s Secret
	for _, verb := range secretVerbs {
		if got := fmt.Sprintf(verb, s); got != "[not set]" {
			t.Errorf("%s printed %q, want %q", verb, got, "[not set]")
		}
	}
}

func TestSecretIsRedactedWhenPrinted(t *testing.T) {
	t.Parallel()
	s := Secret("postgres://gw:hunter2@db/gateway")
	holder := struct{ DatabaseURL Secret }{s} // a Secret inside a struct, as Config will hold one
	for _, verb := range secretVerbs {
		if got := fmt.Sprintf(verb, s); got != "[redacted]" {
			t.Errorf("%s printed %q for a set secret, want %q", verb, got, "[redacted]")
		}
		if out := fmt.Sprintf(verb, holder); strings.Contains(out, "hunter2") {
			t.Errorf("%s printed the secret inside a struct: %s", verb, out)
		}
	}
	if string(s) != "postgres://gw:hunter2@db/gateway" {
		t.Error("an explicit conversion must still return the value")
	}
}
