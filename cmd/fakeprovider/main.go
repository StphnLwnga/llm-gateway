// Command fakeprovider runs the fake OpenAI-compatible upstream for local and
// load tests.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/fakeupstream"
	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
	"github.com/StphnLwnga/llm-gateway/internal/telemetry"
)

const (
	defaultPort = "8081"
	// shutdownTimeout is short because a stalled fake stream never finishes
	// on its own; Close ends it.
	shutdownTimeout = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop) // a second signal kills the process at once
	err := run(ctx, os.LookupEnv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeprovider: %v\n", err)
		os.Exit(1)
	}
}

// run is the whole program; tests call it with fakes.
func run(ctx context.Context, getenv func(string) (string, bool), stdout io.Writer) error {
	port := defaultPort
	if v, ok := getenv("PORT"); ok {
		port = v
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("PORT %q: must be a number from 1 to 65535", port)
	}

	log := telemetry.NewLogger(stdout, slog.LevelInfo, config.FormatJSON)
	srv := httpapi.NewServer(":"+port, fakeupstream.New())
	srv.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelWarn)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.InfoContext(ctx, "listening", "addr", ln.Addr().String())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutdown started", "shutdown_timeout", shutdownTimeout.String())
	// ctx is already cancelled; a deadline derived from it would expire at once.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", errors.Join(err, srv.Close()))
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}
