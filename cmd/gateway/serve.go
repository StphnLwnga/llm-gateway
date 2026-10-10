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

// serve runs srv on ln until ctx is cancelled, then shuts down gracefully:
//
//  1. startDraining makes /readyz return 503, so the load balancer stops
//     sending new requests. The server still accepts and serves them.
//  2. After drainDelay, Shutdown closes the listener and waits for in-flight
//     requests, for up to shutdownTimeout.
//  3. If requests are still running at the deadline, the remaining
//     connections are closed and serve returns the deadline error.
//
// srv must keep its default BaseContext. Request contexts must NOT derive
// from ctx: SIGTERM would then cancel every in-flight request, and every
// stream, at once, which defeats the drain.
func serve(ctx context.Context, log *slog.Logger, ln net.Listener, srv *http.Server,
	startDraining func(), drainDelay, shutdownTimeout time.Duration) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err) // Serve stopped without a shutdown
	case <-ctx.Done():
	}

	log.Info("shutdown started", "drain_delay", drainDelay.String(), "shutdown_timeout", shutdownTimeout.String())
	startDraining()
	time.Sleep(drainDelay)

	// ctx is already cancelled, so the shutdown deadline cannot derive from its
	// cancellation. WithoutCancel keeps ctx's values and drops its Done channel.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		srv.Close() // cut the requests that did not finish in time
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}
