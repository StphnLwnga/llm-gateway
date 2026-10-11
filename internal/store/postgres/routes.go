package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/StphnLwnga/llm-gateway/internal/money"
	"github.com/StphnLwnga/llm-gateway/internal/router"
)

// ErrRouteNotFound is returned when no route has the given ID.
var ErrRouteNotFound = errors.New("route not found")

// Routes reads and writes the routes table.
type Routes struct {
	Pool *pgxpool.Pool
}

// Prices cross the boundary as integer micro-USD; Postgres does the exact
// numeric conversion, so no float is ever involved.
const routeColumns = `id, alias, provider, upstream_model, priority, enabled,
	(input_usd_per_mtok * 1000000)::bigint, (output_usd_per_mtok * 1000000)::bigint,
	default_max_tokens, max_max_tokens, created_at, updated_at`

// EnabledRoutes returns every enabled route.
func (r Routes) EnabledRoutes(ctx context.Context) ([]router.Route, error) {
	return r.query(ctx, "SELECT "+routeColumns+" FROM routes WHERE enabled ORDER BY alias, priority")
}

// List returns every route, enabled or not.
func (r Routes) List(ctx context.Context) ([]router.Route, error) {
	return r.query(ctx, "SELECT "+routeColumns+" FROM routes ORDER BY alias, priority")
}

func (r Routes) query(ctx context.Context, sql string) ([]router.Route, error) {
	rows, err := r.Pool.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("query routes: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (router.Route, error) {
		var rt router.Route
		var in, outPrice int64
		err := row.Scan(&rt.ID, &rt.Alias, &rt.Provider, &rt.UpstreamModel, &rt.Priority, &rt.Enabled,
			&in, &outPrice, &rt.DefaultMaxTokens, &rt.MaxMaxTokens, &rt.CreatedAt, &rt.UpdatedAt)
		rt.InputPrice, rt.OutputPrice = money.MicroUSD(in), money.MicroUSD(outPrice)
		return rt, err
	})
	if err != nil {
		return nil, fmt.Errorf("read routes: %w", err)
	}
	return out, nil
}

// Add inserts a route after validating it, and returns its ID.
func (r Routes) Add(ctx context.Context, rt router.Route) (int64, error) {
	if err := rt.Validate(); err != nil {
		return 0, err
	}
	var id int64
	err := r.Pool.QueryRow(ctx, `INSERT INTO routes
		(alias, provider, upstream_model, priority, enabled, input_usd_per_mtok, output_usd_per_mtok, default_max_tokens, max_max_tokens)
		VALUES ($1, $2, $3, $4, $5, $6::bigint::numeric / 1000000, $7::bigint::numeric / 1000000, $8, $9)
		RETURNING id`,
		rt.Alias, rt.Provider, rt.UpstreamModel, rt.Priority, rt.Enabled, int64(rt.InputPrice), int64(rt.OutputPrice),
		rt.DefaultMaxTokens, rt.MaxMaxTokens).Scan(&id)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return 0, fmt.Errorf("route conflicts with an existing route (%s): %w", pe.ConstraintName, err)
		}
		return 0, fmt.Errorf("insert route: %w", err)
	}
	return id, nil
}

// SetEnabled enables or disables the route with the given ID.
func (r Routes) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	tag, err := r.Pool.Exec(ctx, "UPDATE routes SET enabled = $2, updated_at = now() WHERE id = $1", id, enabled)
	if err != nil {
		return fmt.Errorf("update route: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: id %d", ErrRouteNotFound, id)
	}
	return nil
}
