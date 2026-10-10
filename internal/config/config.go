// Package config loads the gateway's settings once, at startup, into typed values.
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Secret holds a sensitive value. It prints as [redacted], or [not set] when empty, under every fmt verb.
type Secret string

// String implements fmt.Stringer and prints [not set] when the value is missing.
func (s Secret) String() string {
	if s == "" {
		return "[not set]"
	}
	return "[redacted]"
}

// Format implements fmt.Formatter, so every verb, including %d and %x, prints the placeholder.
// Only package fmt calls it; slog's JSON handler and encoding/json do not.
func (s Secret) Format(f fmt.State, verb rune) {
	io.WriteString(f, s.String())
}

// LogValue implements slog.LogValuer. Package log/slog calls it when it resolves
// an attribute, before any handler sees the value, so every handler gets the placeholder.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue(s.String())
}

// MarshalText implements encoding.TextMarshaler. Package encoding/json calls it,
// including for a Secret inside a struct that slog's JSON handler marshals, and
// slog's text handler calls it for values it does not format itself.
func (s Secret) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

// Log output formats accepted by LOG_FORMAT.
const (
	FormatJSON = "json"
	FormatText = "text"
)

// Config holds every setting the gateway reads, already parsed into its real type.
type Config struct {
	Port      int
	LogLevel  slog.Level
	LogFormat string // FormatJSON or FormatText

	// DrainDelay is how long the server keeps serving, with /readyz at 503, after
	// SIGTERM and before it stops accepting connections. It gives the load balancer
	// time to stop sending new requests. Kubernetes sets about 5s (S8).
	DrainDelay time.Duration
	// ShutdownTimeout bounds how long in-flight requests get to finish after the
	// drain. DrainDelay plus ShutdownTimeout must stay below Kubernetes' 30s
	// grace period, or SIGKILL cuts the shutdown short.
	ShutdownTimeout time.Duration
}

// Addr is the listen address for the HTTP server.
func (c Config) Addr() string {
	return fmt.Sprintf(":%d", c.Port)
}

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// Load reads each setting through lookup, applies defaults to unset values,
// parses each value into its type, and reports every invalid setting together.
// Pass os.LookupEnv in main, and a fake in tests.
func Load(lookup func(key string) (string, bool)) (Config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok {
			return v
		}
		return def
	}

	var cfg Config
	var errs []error

	rawPort := get("PORT", "8080")
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		errs = append(errs, fmt.Errorf("PORT %q: must be a number from 1 to 65535", rawPort))
	}
	cfg.Port = port

	rawLevel := get("LOG_LEVEL", "info")
	level, ok := logLevels[strings.ToLower(rawLevel)]
	if !ok {
		errs = append(errs, fmt.Errorf("LOG_LEVEL %q: must be debug, info, warn or error", rawLevel))
	}
	cfg.LogLevel = level

	cfg.LogFormat = strings.ToLower(get("LOG_FORMAT", FormatJSON))
	if cfg.LogFormat != FormatJSON && cfg.LogFormat != FormatText {
		errs = append(errs, fmt.Errorf("LOG_FORMAT %q: must be json or text", cfg.LogFormat))
	}

	duration := func(key, def string) time.Duration {
		raw := get(key, def)
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("%s %q: must be a duration of 0 or more, for example 5s", key, raw))
		}
		return d
	}
	cfg.DrainDelay = duration("DRAIN_DELAY", "0s")
	cfg.ShutdownTimeout = duration("SHUTDOWN_TIMEOUT", "20s")

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
