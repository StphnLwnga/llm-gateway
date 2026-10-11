package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const testDSN = "postgres://gw:hunter2@db:5432/gateway"

// env returns a map-backed lookup that sets DATABASE_URL unless vars does.
func env(vars map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if v, ok := vars[key]; ok {
			return v, ok
		}
		if key == "DATABASE_URL" {
			return testDSN, true
		}
		return "", false
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
	if cfg.DrainDelay != 0 {
		t.Errorf("DrainDelay = %v, want 0s", cfg.DrainDelay)
	}
	if cfg.ShutdownTimeout != 20*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 20s", cfg.ShutdownTimeout)
	}
	if cfg.DBMaxConns != 10 {
		t.Errorf("DBMaxConns = %d, want 10", cfg.DBMaxConns)
	}
	if cfg.ProviderConnectTimeout != 5*time.Second {
		t.Errorf("ProviderConnectTimeout = %v, want 5s", cfg.ProviderConnectTimeout)
	}
	if cfg.ProviderFirstByteTimeout != 60*time.Second {
		t.Errorf("ProviderFirstByteTimeout = %v, want 60s", cfg.ProviderFirstByteTimeout)
	}
	if cfg.GroqAPIKey != "" || cfg.OpenAIAPIKey != "" {
		t.Errorf("provider keys are set by default")
	}
	if cfg.OllamaBaseURL != "" {
		t.Errorf("OllamaBaseURL = %q by default, want empty (Ollama disabled)", cfg.OllamaBaseURL)
	}
	if cfg.GroqBaseURL != "https://api.groq.com/openai/v1" || cfg.OpenAIBaseURL != "https://api.openai.com/v1" {
		t.Errorf("GroqBaseURL, OpenAIBaseURL = %q, %q; want the public endpoints", cfg.GroqBaseURL, cfg.OpenAIBaseURL)
	}
	if string(cfg.DatabaseURL) != testDSN {
		t.Errorf("DatabaseURL was not read")
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Parallel()
	missing := func(string) (string, bool) { return "", false }
	_, err := Load(missing)
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("Load without DATABASE_URL = %v, want an error naming DATABASE_URL", err)
	}
}

func TestDatabaseURLErrorNeverPrintsTheValue(t *testing.T) {
	t.Parallel()
	_, err := Load(env(map[string]string{"DB_MAX_CONNS": "0"}))
	if err == nil {
		t.Fatal("Load succeeded, want an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error printed the database password: %v", err)
	}
	cfg, _ := Load(env(nil))
	if out := fmt.Sprintf("%+v", cfg); strings.Contains(out, "hunter2") {
		t.Errorf("%%+v of Config printed the database password: %s", out)
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

func TestLoadBoundsDBMaxConns(t *testing.T) {
	t.Parallel()
	cfg, err := Load(env(map[string]string{"DB_MAX_CONNS": "1000"}))
	if err != nil {
		t.Fatalf("Load with DB_MAX_CONNS=1000: %v", err)
	}
	if cfg.DBMaxConns != 1000 {
		t.Errorf("DBMaxConns = %d, want 1000", cfg.DBMaxConns)
	}
	_, err = Load(env(map[string]string{"DB_MAX_CONNS": "1001"}))
	if err == nil || !strings.Contains(err.Error(), "from 1 to 1000") {
		t.Errorf("Load with DB_MAX_CONNS=1001 = %v, want an error naming the range 1 to 1000", err)
	}
}

func TestLoadParsesDurations(t *testing.T) {
	t.Parallel()
	cases := []struct {
		drain, shutdown         string
		wantDrain, wantShutdown time.Duration
	}{
		{"0s", "0s", 0, 0},
		{"5s", "20s", 5 * time.Second, 20 * time.Second},
		{"500ms", "1m30s", 500 * time.Millisecond, 90 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.drain+"/"+tc.shutdown, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(env(map[string]string{"DRAIN_DELAY": tc.drain, "SHUTDOWN_TIMEOUT": tc.shutdown}))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DrainDelay != tc.wantDrain || cfg.ShutdownTimeout != tc.wantShutdown {
				t.Errorf("DrainDelay, ShutdownTimeout = %v, %v; want %v, %v",
					cfg.DrainDelay, cfg.ShutdownTimeout, tc.wantDrain, tc.wantShutdown)
			}
		})
	}
}

func TestLoadParsesProviderTimeouts(t *testing.T) {
	t.Parallel()
	cfg, err := Load(env(map[string]string{"PROVIDER_CONNECT_TIMEOUT": "2s", "PROVIDER_FIRST_BYTE_TIMEOUT": "1m30s"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProviderConnectTimeout != 2*time.Second || cfg.ProviderFirstByteTimeout != 90*time.Second {
		t.Errorf("ProviderConnectTimeout, ProviderFirstByteTimeout = %v, %v; want 2s, 1m30s",
			cfg.ProviderConnectTimeout, cfg.ProviderFirstByteTimeout)
	}
}

func TestLoadReadsProviderKeysAndBaseURLs(t *testing.T) {
	t.Parallel()
	cfg, err := Load(env(map[string]string{
		"GROQ_API_KEY": "gsk_test", "OPENAI_API_KEY": "sk-test",
		"GROQ_BASE_URL": "http://fakeprovider:8081/v1", "OPENAI_BASE_URL": "https://proxy.example/v1",
		"OLLAMA_BASE_URL": "http://host.docker.internal:11434",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GroqAPIKey != "gsk_test" || cfg.OpenAIAPIKey != "sk-test" {
		t.Error("provider keys were not read")
	}
	if cfg.OllamaBaseURL != "http://host.docker.internal:11434" {
		t.Errorf("OllamaBaseURL = %q", cfg.OllamaBaseURL)
	}
	if cfg.GroqBaseURL != "http://fakeprovider:8081/v1" || cfg.OpenAIBaseURL != "https://proxy.example/v1" {
		t.Errorf("GroqBaseURL, OpenAIBaseURL = %q, %q", cfg.GroqBaseURL, cfg.OpenAIBaseURL)
	}
	if out := fmt.Sprintf("%+v", cfg); strings.Contains(out, "gsk_test") || strings.Contains(out, "sk-test") {
		t.Errorf("%%+v of Config printed a provider key: %s", out)
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
		{"drain delay without a unit", map[string]string{"DRAIN_DELAY": "5"}, "DRAIN_DELAY"},
		{"drain delay negative", map[string]string{"DRAIN_DELAY": "-1s"}, "DRAIN_DELAY"},
		{"drain delay set but empty", map[string]string{"DRAIN_DELAY": ""}, "DRAIN_DELAY"},
		{"shutdown timeout not a duration", map[string]string{"SHUTDOWN_TIMEOUT": "soon"}, "SHUTDOWN_TIMEOUT"},
		{"shutdown timeout negative", map[string]string{"SHUTDOWN_TIMEOUT": "-20s"}, "SHUTDOWN_TIMEOUT"},
		{"connect timeout zero", map[string]string{"PROVIDER_CONNECT_TIMEOUT": "0s"}, "PROVIDER_CONNECT_TIMEOUT"},
		{"connect timeout negative", map[string]string{"PROVIDER_CONNECT_TIMEOUT": "-5s"}, "PROVIDER_CONNECT_TIMEOUT"},
		{"connect timeout without a unit", map[string]string{"PROVIDER_CONNECT_TIMEOUT": "5"}, "PROVIDER_CONNECT_TIMEOUT"},
		{"first byte timeout zero", map[string]string{"PROVIDER_FIRST_BYTE_TIMEOUT": "0s"}, "PROVIDER_FIRST_BYTE_TIMEOUT"},
		{"first byte timeout set but empty", map[string]string{"PROVIDER_FIRST_BYTE_TIMEOUT": ""}, "PROVIDER_FIRST_BYTE_TIMEOUT"},
		{"groq base url without a scheme", map[string]string{"GROQ_BASE_URL": "api.groq.com/openai/v1"}, "GROQ_BASE_URL"},
		{"openai base url with another scheme", map[string]string{"OPENAI_BASE_URL": "ftp://api.openai.com/v1"}, "OPENAI_BASE_URL"},
		{"ollama base url set but empty", map[string]string{"OLLAMA_BASE_URL": ""}, "OLLAMA_BASE_URL"},
		{"ollama base url without a scheme", map[string]string{"OLLAMA_BASE_URL": "localhost:11434"}, "OLLAMA_BASE_URL"},
		{"openai base url set but empty", map[string]string{"OPENAI_BASE_URL": ""}, "OPENAI_BASE_URL"},
		{"database url set but empty", map[string]string{"DATABASE_URL": ""}, "DATABASE_URL"},
		{"max conns zero", map[string]string{"DB_MAX_CONNS": "0"}, "DB_MAX_CONNS"},
		{"max conns negative", map[string]string{"DB_MAX_CONNS": "-3"}, "DB_MAX_CONNS"},
		{"max conns not a number", map[string]string{"DB_MAX_CONNS": "ten"}, "DB_MAX_CONNS"},
		{"max conns above the bound", map[string]string{"DB_MAX_CONNS": "1001"}, "DB_MAX_CONNS"},
		{"max conns that wraps to 1 as int32", map[string]string{"DB_MAX_CONNS": "4294967297"}, "DB_MAX_CONNS"},
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
	_, err := Load(env(map[string]string{"PORT": "x", "LOG_LEVEL": "loud", "LOG_FORMAT": "xml", "DATABASE_URL": ""}))
	if err == nil {
		t.Fatal("Load succeeded, want an error")
	}
	for _, key := range []string{"PORT", "LOG_LEVEL", "LOG_FORMAT", "DATABASE_URL"} {
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

// TestSecretIsRedactedOutsideFmt covers paths the fmt tests cannot: each package checks only its own hook.
func TestSecretIsRedactedOutsideFmt(t *testing.T) {
	t.Parallel()
	const raw = "postgres://gw:hunter2@db/gateway"
	s := Secret(raw)
	holder := struct{ DatabaseURL Secret }{s}

	logWith := func(newHandler func(*bytes.Buffer) slog.Handler, args ...any) string {
		var buf bytes.Buffer
		slog.New(newHandler(&buf)).Info("config loaded", args...)
		return buf.String()
	}
	jsonHandler := func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) }
	textHandler := func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) }
	marshalled, err := json.Marshal(holder)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	cases := []struct {
		name string
		out  string
	}{
		{"slog JSON handler, Secret as an attr", logWith(jsonHandler, "database_url", s)},
		{"slog JSON handler, struct holding a Secret", logWith(jsonHandler, "config", holder)},
		{"slog text handler, Secret as an attr", logWith(textHandler, "database_url", s)},
		{"slog text handler, struct holding a Secret", logWith(textHandler, "config", holder)},
		{"json.Marshal of a struct holding a Secret", string(marshalled)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.out, "hunter2") {
				t.Errorf("output contains the raw value: %s", tc.out)
			}
			if !strings.Contains(tc.out, "[redacted]") {
				t.Errorf("output is missing [redacted]: %s", tc.out)
			}
		})
	}
}

// TestSecretResolvesToPlaceholderInSlog covers handlers that never call MarshalText.
func TestSecretResolvesToPlaceholderInSlog(t *testing.T) {
	t.Parallel()
	v := slog.AnyValue(Secret("hunter2")).Resolve()
	if v.Kind() != slog.KindString || v.String() != "[redacted]" {
		t.Errorf("resolved value = %v (kind %v), want the string [redacted]", v, v.Kind())
	}
}
