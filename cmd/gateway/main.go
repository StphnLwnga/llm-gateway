// Command gateway runs the llm-gateway HTTP server.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
	"github.com/StphnLwnga/llm-gateway/internal/store/postgres"
	"github.com/StphnLwnga/llm-gateway/internal/telemetry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first signal, stop unregisters the handler and the default
	// signal behaviour returns, so a second signal kills the process at once
	// instead of waiting out the drain and shutdown.
	context.AfterFunc(ctx, stop)
	err := run(ctx, os.LookupEnv, os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
}

// run is the whole program. main passes the real process environment and
// streams; tests pass fakes. ctx is cancelled on SIGINT or SIGTERM, which
// starts the graceful shutdown in serve. stderr is unused: logs go to stdout,
// and main prints run's error to stderr.
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
		"drain_delay", cfg.DrainDelay.String(),
		"shutdown_timeout", cfg.ShutdownTimeout.String(),
		"db_max_conns", cfg.DBMaxConns,
	)

	// The pool connects lazily, so the gateway starts even when Postgres is
	// down; /readyz reports 503 until Postgres answers.
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close() // after serve returns, so in-flight requests keep their connections

	api := httpapi.New(log, postgres.Pinger{Pool: pool})
	srv := httpapi.NewServer(cfg.Addr(), api)
	srv.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelWarn)

	ln, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.InfoContext(ctx, "listening", "addr", ln.Addr().String())
	return serve(ctx, log, ln, srv, api.StartDraining, cfg.DrainDelay, cfg.ShutdownTimeout)
}
