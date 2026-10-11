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

// Secret holds a sensitive value. It prints as [redacted], or [not set] when
// empty, through fmt, log/slog and encoding/json; each needs its own method below.
type Secret string

// String implements fmt.Stringer.
func (s Secret) String() string {
	if s == "" {
		return "[not set]"
	}
	return "[redacted]"
}

// Format implements fmt.Formatter, so every verb prints the placeholder.
func (s Secret) Format(f fmt.State, verb rune) {
	_, _ = io.WriteString(f, s.String())
}

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue(s.String())
}

// MarshalText implements encoding.TextMarshaler.
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

	// DrainDelay is how long /readyz reports 503 before the listener closes.
	DrainDelay time.Duration
	// ShutdownTimeout bounds how long in-flight requests get after the drain.
	ShutdownTimeout time.Duration

	DatabaseURL Secret
	DBMaxConns  int
}

// Addr is the listen address for the HTTP server.
func (c Config) Addr() string {
	return fmt.Sprintf(":%d", c.Port)
}

// maxDBConns keeps DB_MAX_CONNS far below Postgres' connection limit and inside int32.
const maxDBConns = 1000

var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// Load parses every setting through lookup and reports all invalid ones together.
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

	if v, ok := lookup("DATABASE_URL"); ok && v != "" {
		cfg.DatabaseURL = Secret(v)
	} else {
		errs = append(errs, errors.New("DATABASE_URL: required, for example postgres://user:password@host:5432/db"))
	}

	rawMaxConns := get("DB_MAX_CONNS", "10")
	maxConns, err := strconv.Atoi(rawMaxConns)
	if err != nil || maxConns < 1 || maxConns > maxDBConns {
		errs = append(errs, fmt.Errorf("DB_MAX_CONNS %q: must be a whole number from 1 to %d", rawMaxConns, maxDBConns))
	}
	cfg.DBMaxConns = maxConns

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
