// Package postgres is the gateway's Postgres store.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/StphnLwnga/llm-gateway/internal/config"
)

// Open returns a pool of at most maxConns connections without dialing Postgres.
func Open(ctx context.Context, dsn config.Secret, maxConns int) (*pgxpool.Pool, error) {
	if maxConns < 1 || maxConns > math.MaxInt32 {
		return nil, fmt.Errorf("pool size %d: must be from 1 to %d", maxConns, math.MaxInt32)
	}
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

// Pinger is the Postgres readiness check.
type Pinger struct {
	Pool *pgxpool.Pool
}

// Check pings Postgres on a pooled connection.
func (p Pinger) Check(ctx context.Context) error {
	if err := p.Pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres ping: %w", err)
	}
	return nil
}
