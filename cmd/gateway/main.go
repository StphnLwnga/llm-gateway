// Command gateway runs the llm-gateway HTTP server.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/telemetry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.LookupEnv, os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
}

// run is the whole program. main passes the real process environment and
// streams; tests pass fakes. ctx is cancelled on SIGINT or SIGTERM.
// stderr is unused until the server arrives; main prints run's error there.
func run(ctx context.Context, getenv func(string) (string, bool), stdout, stderr io.Writer) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}

	log := telemetry.NewLogger(stdout, cfg.LogLevel, cfg.LogFormat)
	log.InfoContext(ctx, "config loaded",
		"port", cfg.Port,
		"log_level", cfg.LogLevel.String(),
		"log_format", cfg.LogFormat,
	)
	return nil
}
