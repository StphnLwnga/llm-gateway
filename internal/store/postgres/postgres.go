// Package postgres is the gateway's Postgres store.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/StphnLwnga/llm-gateway/internal/config"
)

// ErrUnsafeRole is returned when the gateway's database role could bypass row-level security.
var ErrUnsafeRole = errors.New("unsafe database role for the gateway")

// Open returns the gateway's pool of at most maxConns connections without
// dialing Postgres; every new connection must pass checkRole.
func Open(ctx context.Context, dsn config.Secret, maxConns int) (*pgxpool.Pool, error) {
	return open(ctx, dsn, maxConns, checkRole)
}

// OpenAdmin is Open without the role check, for gatewayctl's admin role.
func OpenAdmin(ctx context.Context, dsn config.Secret, maxConns int) (*pgxpool.Pool, error) {
	return open(ctx, dsn, maxConns, nil)
}

func open(ctx context.Context, dsn config.Secret, maxConns int, afterConnect func(context.Context, *pgx.Conn) error) (*pgxpool.Pool, error) {
	if maxConns < 1 || maxConns > math.MaxInt32 {
		return nil, fmt.Errorf("pool size %d: must be from 1 to %d", maxConns, math.MaxInt32)
	}
	pc, err := pgxpool.ParseConfig(string(dsn))
	if err != nil {
		// The parse error can quote the connection string, password included.
		return nil, errors.New("parse DATABASE_URL: not a valid Postgres connection string")
	}
	pc.MaxConns = int32(maxConns)
	pc.AfterConnect = afterConnect
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

// A role bypasses row-level security if it is a superuser, has BYPASSRLS or
// owns the table. MEMBER covers every role the login role can SET ROLE to,
// because any of those is one statement away.
const roleRulesSQL = `SELECT session_user,
	coalesce(bool_or(r.rolsuper), false),
	coalesce(bool_or(r.rolbypassrls), false),
	coalesce(bool_or(EXISTS (
		SELECT 1 FROM pg_class c
		WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'p') AND c.relowner = r.oid
	)), false)
FROM pg_roles r
WHERE pg_has_role(session_user, r.oid, 'MEMBER')`

func checkRole(ctx context.Context, conn *pgx.Conn) error {
	var role string
	var superuser, bypassRLS, owner bool
	if err := conn.QueryRow(ctx, roleRulesSQL).Scan(&role, &superuser, &bypassRLS, &owner); err != nil {
		return fmt.Errorf("check database role: %w", err)
	}
	var failed []string
	if superuser {
		failed = append(failed, "is a superuser")
	}
	if bypassRLS {
		failed = append(failed, "has BYPASSRLS")
	}
	if owner {
		failed = append(failed, "owns a table in schema public")
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w: role %q, or a role it can become, %s", ErrUnsafeRole, role, strings.Join(failed, ", "))
	}
	return nil
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
