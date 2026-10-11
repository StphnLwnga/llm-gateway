// Command gateway runs the llm-gateway HTTP server.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/store/postgres"
	"github.com/StphnLwnga/llm-gateway/internal/telemetry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop) // a second signal kills the process at once
	err := run(ctx, os.LookupEnv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
}

// run is the whole program; tests call it with fakes.
func run(ctx context.Context, getenv func(string) (string, bool), stdout io.Writer) error {
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
		"provider_connect_timeout", cfg.ProviderConnectTimeout.String(),
		"provider_first_byte_timeout", cfg.ProviderFirstByteTimeout.String(),
	)

	client := provider.NewHTTPClient(cfg.ProviderConnectTimeout, cfg.ProviderFirstByteTimeout)
	registry := providers(ctx, cfg, client, log)
	log.InfoContext(ctx, "providers ready", "names", slices.Sorted(maps.Keys(registry)))

	pool, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	api := httpapi.New(log, postgres.Pinger{Pool: pool})
	srv := httpapi.NewServer(cfg.Addr(), api)
	srv.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelWarn)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Addr())
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.InfoContext(ctx, "listening", "addr", ln.Addr().String())
	return serve(ctx, log, ln, srv, api.StartDraining, cfg.DrainDelay, cfg.ShutdownTimeout)
}
