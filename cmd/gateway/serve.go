package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// serve runs srv on ln until ctx is cancelled, then drains, shuts down and
// flushes what the last requests recorded. srv must keep its default
// BaseContext: request contexts derived from ctx would all end on SIGTERM.
func serve(ctx context.Context, log *slog.Logger, ln net.Listener, srv *http.Server,
	startDraining func(), drainDelay, shutdownTimeout time.Duration, flush func(context.Context) error) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutdown started", "drain_delay", drainDelay.String(), "shutdown_timeout", shutdownTimeout.String())
	startDraining()
	time.Sleep(drainDelay)

	// ctx is already cancelled; a deadline derived from it would expire at once.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	// After Shutdown, so the spans of the last requests are in the flush; a
	// failed export must not turn a clean shutdown into an error exit.
	if err := flush(shutdownCtx); err != nil {
		log.Warn("telemetry flush failed", "error", err)
	}
	if shutdownErr != nil {
		return fmt.Errorf("shutdown: %w", errors.Join(shutdownErr, srv.Close()))
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}
