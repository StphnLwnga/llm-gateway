package postgres_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func isPermissionDenied(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42501"
}

func TestAppRoleOnlyReadsRoutes(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	ctx := t.Context()
	app := openPool(t, d.App)

	if _, err := app.Exec(ctx, "SELECT * FROM routes"); err != nil {
		t.Errorf("app SELECT on routes: %v", err)
	}
	for name, sql := range map[string]string{
		"insert a route":        `INSERT INTO routes (alias, provider, upstream_model, priority) VALUES ('x', 'groq', 'm', 1)`,
		"create a table":        `CREATE TABLE app_made (i int)`,
		"read goose_db_version": `SELECT * FROM goose_db_version`,
	} {
		if _, err := app.Exec(ctx, sql); !isPermissionDenied(err) {
			t.Errorf("app can %s: %v, want permission denied", name, err)
		}
	}
}

func TestAdminRoleManagesRoutes(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	ctx := t.Context()
	admin := openPool(t, d.Admin)

	var id int64
	if err := admin.QueryRow(ctx, `INSERT INTO routes (alias, provider, upstream_model, priority)
		VALUES ('x', 'groq', 'm', 1) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("admin INSERT on routes: %v", err)
	}
	if tag, err := admin.Exec(ctx, "UPDATE routes SET enabled = false WHERE id = $1", id); err != nil || tag.RowsAffected() != 1 {
		t.Errorf("admin UPDATE on routes = %v, %v; want one row", tag, err)
	}
	if tag, err := admin.Exec(ctx, "DELETE FROM routes WHERE id = $1", id); err != nil || tag.RowsAffected() != 1 {
		t.Errorf("admin DELETE on routes = %v, %v; want one row", tag, err)
	}
}
