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
	"runtime/debug"
	"slices"
	"syscall"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
	"github.com/StphnLwnga/llm-gateway/internal/provider"
	"github.com/StphnLwnga/llm-gateway/internal/router"
	"github.com/StphnLwnga/llm-gateway/internal/store/postgres"
	"github.com/StphnLwnga/llm-gateway/internal/telemetry"
	"github.com/StphnLwnga/llm-gateway/internal/telemetry/otelsdk"
)

// version is set at build time with -ldflags "-X main.version=..."; the image build passes VERSION.
var version string

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
		"routes_refresh_interval", cfg.RoutesRefreshInterval.String(),
		"provider_first_token_timeout", cfg.ProviderFirstTokenTimeout.String(),
		"stream_stall_timeout", cfg.StreamStallTimeout.String(),
		"status_hold", cfg.StatusHold.String(),
		"heartbeat_interval", cfg.HeartbeatInterval.String(),
		"request_max_bytes", cfg.RequestMaxBytes,
	)

	tel, err := otelsdk.New(ctx, cfg.OTLPEndpoint, buildVersion())
	if err != nil {
		return err
	}
	otel.SetTracerProvider(tel.Tracer)
	otel.SetMeterProvider(tel.Meter)
	// No Baggage: a client's baggage would be forwarded to third-party providers.
	prop := propagation.TraceContext{}
	otel.SetTextMapPropagator(prop)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Warn("telemetry export failed", "error", err)
	}))
	log.InfoContext(ctx, "telemetry ready", "export", cfg.OTLPEndpoint != "")

	client := provider.Instrument(provider.NewHTTPClient(cfg.ProviderConnectTimeout, cfg.ProviderFirstByteTimeout),
		tel.Tracer, tel.Meter, prop)
	registry, err := providers(ctx, cfg, client, tel.Tracer, tel.Meter, log)
	if err != nil {
		return fmt.Errorf("provider telemetry: %w", err)
	}
	log.InfoContext(ctx, "providers ready", "names", slices.Sorted(maps.Keys(registry)))

	pool, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	routes := router.New(postgres.Routes{Pool: pool}, registry, cfg.RoutesRefreshInterval, log)
	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		routes.Run(ctx)
	}()
	defer func() { <-refreshed }() // runs before pool.Close, so no refresh is left using the pool

	api := httpapi.New(log, httpapi.Deps{
		Models: routing{routes},
		Routes: routing{routes},
		Chat:   httpapi.ChatConfig{StatusHold: cfg.StatusHold, HeartbeatInterval: cfg.HeartbeatInterval, MaxBodyBytes: cfg.RequestMaxBytes},
		Telemetry: httpapi.Telemetry{
			Tracer:     tel.Tracer,
			Meter:      tel.Meter,
			Propagator: prop,
			ServerAddr: fmt.Sprintf("%s:%d", otelsdk.ServiceName, cfg.Port),
		},
	}, postgres.Pinger{Pool: pool}, routes)
	srv := httpapi.NewServer(cfg.Addr(), api)
	srv.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelWarn)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Addr())
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.InfoContext(ctx, "listening", "addr", ln.Addr().String())
	return serve(ctx, log, ln, srv, api.StartDraining, cfg.DrainDelay, cfg.ShutdownTimeout, tel.Shutdown)
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
		return bi.Main.Version
	}
	return "unknown"
}
