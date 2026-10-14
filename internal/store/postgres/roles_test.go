package postgres_test

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
	"github.com/StphnLwnga/llm-gateway/internal/store/postgres"
)

func isPermissionDenied(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42501"
}

func TestAppRoleOnlyReadsRoutes(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	ctx := t.Context()
	app := openGatewayPool(t, d.App)

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

const (
	ruleSuperuser = "is a superuser"
	ruleBypassRLS = "has BYPASSRLS"
	ruleOwner     = "owns a table in schema public"
)

func TestGatewayRefusesRolesThatBypassRowLevelSecurity(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	ctx := t.Context()
	const pw = "member-test"
	// table_owner owns a table but has no BYPASSRLS, so only the owner rule catches its members.
	if _, err := openPool(t, d.Superuser).Exec(ctx, `
		CREATE ROLE migrator_member LOGIN PASSWORD '`+pw+`' IN ROLE gateway_migrator;
		CREATE ROLE table_owner NOLOGIN;
		CREATE TABLE owned (i int);
		ALTER TABLE owned OWNER TO table_owner;
		CREATE ROLE owner_member LOGIN PASSWORD '`+pw+`';
		GRANT table_owner TO owner_member;
		CREATE ROLE owner_set_only LOGIN PASSWORD '`+pw+`';
		GRANT table_owner TO owner_set_only WITH INHERIT FALSE;
		GRANT CONNECT ON DATABASE gateway TO migrator_member, owner_member, owner_set_only;
	`); err != nil {
		t.Fatalf("create test roles: %v", err)
	}

	for _, tc := range []struct {
		role, password string
		want           []string
	}{
		{"postgres", superuserPassword, []string{ruleSuperuser, ruleBypassRLS, ruleOwner}},
		{"gateway_migrator", migratorPassword, []string{ruleBypassRLS, ruleOwner}},
		{"gateway_admin", adminPassword, []string{ruleBypassRLS}},
		{"migrator_member", pw, []string{ruleBypassRLS, ruleOwner}},
		{"owner_member", pw, []string{ruleOwner}},
		{"owner_set_only", pw, []string{ruleOwner}},
		{"gateway_app", appPassword, nil},
	} {
		t.Run(tc.role, func(t *testing.T) {
			err := postgres.Pinger{Pool: openGatewayPool(t, d.dsn(tc.role, tc.password))}.Check(ctx)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("gateway refused %s: %v", tc.role, err)
				}
				return
			}
			if !errors.Is(err, postgres.ErrUnsafeRole) {
				t.Fatalf("gateway connected as %s: %v, want ErrUnsafeRole", tc.role, err)
			}
			var got []string
			for _, rule := range []string{ruleSuperuser, ruleBypassRLS, ruleOwner} {
				if strings.Contains(err.Error(), rule) {
					got = append(got, rule)
				}
			}
			if !slices.Equal(got, tc.want) || !strings.Contains(err.Error(), `"`+tc.role+`"`) {
				t.Errorf("error = %q, want role %q and rules %q", err, tc.role, tc.want)
			}
		})
	}
}

func TestReadyzLogsTheRefusedRoleWithoutThePassword(t *testing.T) {
	t.Parallel()
	d := migrated(t)
	var logs strings.Builder
	api := httpapi.New(slog.New(slog.NewTextHandler(&logs, nil)), httpapi.Deps{}, postgres.Pinger{Pool: openGatewayPool(t, d.Admin)})

	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz as gateway_admin = %d, want 503", rec.Code)
	}
	if !strings.Contains(logs.String(), ruleBypassRLS) || strings.Contains(logs.String(), adminPassword) {
		t.Errorf("readiness log must name the rule and never the password:\n%s", logs.String())
	}
}
