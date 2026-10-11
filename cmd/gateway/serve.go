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

// serve runs srv on ln until ctx is cancelled, then drains and shuts down.
// srv must keep its default BaseContext: request contexts derived from ctx
// would all end on SIGTERM.
func serve(ctx context.Context, log *slog.Logger, ln net.Listener, srv *http.Server,
	startDraining func(), drainDelay, shutdownTimeout time.Duration) error {
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
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", errors.Join(err, srv.Close()))
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}
