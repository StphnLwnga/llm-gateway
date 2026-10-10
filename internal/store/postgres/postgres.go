// Package postgres is the gateway's Postgres store: the connection pool,
// the readiness check, and the embedded schema migrations.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/StphnLwnga/llm-gateway/internal/config"
)

// Open returns a connection pool for dsn with at most maxConns connections.
//
// pgxpool connects lazily: Open parses the settings and returns without
// dialing Postgres. So the gateway starts even when Postgres is down, and
// GET /readyz reports it until Postgres answers. The start order of services
// then does not matter, which is what Compose and Kubernetes need.
func Open(ctx context.Context, dsn config.Secret, maxConns int) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(string(dsn))
	if err != nil {
		// The parse error can quote the connection string, password included.
		return nil, errors.New("parse DATABASE_URL: not a valid Postgres connection string")
	}
	pc.MaxConns = int32(maxConns)
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

// Pinger is the Postgres readiness check. It implements httpapi.Checker
// without importing httpapi: the interface belongs to the code that uses it.
type Pinger struct {
	Pool *pgxpool.Pool
}

// Check returns nil when Postgres answers a ping on a pooled connection.
func (p Pinger) Check(ctx context.Context) error {
	if err := p.Pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres ping: %w", err)
	}
	return nil
}
