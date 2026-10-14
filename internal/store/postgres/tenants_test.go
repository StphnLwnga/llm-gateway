package postgres_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/StphnLwnga/llm-gateway/internal/store/postgres"
	"github.com/StphnLwnga/llm-gateway/internal/tenant"
)

// tenantsDB has two tenants, a and b, with one key each, and a test-only
// role, reader, that has SELECT on both tables and no way around their policies.
type tenantsDB struct {
	testDB
	admin      postgres.Tenants
	a, b       string // tenant IDs
	keyA, keyB string // full keys
	idA, idB   string // key IDs
}

const readerPassword = "reader-test"

func newTenantsDB(t *testing.T) tenantsDB {
	t.Helper()
	ctx := t.Context()
	d := tenantsDB{testDB: migrated(t)}
	d.admin = postgres.Tenants{Pool: openPool(t, d.Admin)}
	var err error
	if d.a, err = d.admin.Create(ctx, "tenant-a"); err != nil {
		t.Fatalf("create tenant a: %v", err)
	}
	if d.b, err = d.admin.Create(ctx, "tenant-b"); err != nil {
		t.Fatalf("create tenant b: %v", err)
	}
	ka, keyA, err := d.admin.CreateKey(ctx, d.a)
	if err != nil {
		t.Fatalf("create key a: %v", err)
	}
	kb, keyB, err := d.admin.CreateKey(ctx, d.b)
	if err != nil {
		t.Fatalf("create key b: %v", err)
	}
	d.keyA, d.idA, d.keyB, d.idB = keyA, ka.ID, keyB, kb.ID
	if _, err := openPool(t, d.Superuser).Exec(ctx, `
		CREATE ROLE reader LOGIN PASSWORD '`+readerPassword+`';
		GRANT CONNECT ON DATABASE gateway TO reader;
		GRANT SELECT ON tenants, api_keys TO reader;
	`); err != nil {
		t.Fatalf("create reader role: %v", err)
	}
	return d
}

// visible returns the tenant IDs of the rows q can see in tenants and api_keys.
func visible(t *testing.T, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}) (tenants, keyTenants []string) {
	t.Helper()
	read := func(sql string) []string {
		rows, err := q.Query(t.Context(), sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return ids
	}
	return read("SELECT id::text FROM tenants ORDER BY id"), read("SELECT tenant_id::text FROM api_keys ORDER BY id")
}

func TestRowLevelSecurityWithNoTenantShowsNothing(t *testing.T) {
	t.Parallel()
	d := newTenantsDB(t)
	tenants, keys := visible(t, openGatewayPool(t, d.dsn("reader", readerPassword)))
	if len(tenants) != 0 || len(keys) != 0 {
		t.Errorf("reader with no tenant sees tenants %v and keys of %v, want nothing", tenants, keys)
	}
}

func TestRowLevelSecurityShowsOnlyTheTenantsRows(t *testing.T) {
	t.Parallel()
	d := newTenantsDB(t)
	pool := openGatewayPool(t, d.dsn("reader", readerPassword))
	for _, id := range []string{d.a, d.b} {
		err := postgres.InTenant(t.Context(), pool, id, func(tx pgx.Tx) error {
			tenants, keys := visible(t, tx)
			if !slices.Equal(tenants, []string{id}) || !slices.Equal(keys, []string{id}) {
				t.Errorf("in tenant %s: tenants %v, keys of %v; want only %s", id, tenants, keys, id)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("InTenant: %v", err)
		}
	}
}

func TestTenantSettingEndsWithItsTransaction(t *testing.T) {
	t.Parallel()
	d := newTenantsDB(t)
	ctx := t.Context()
	// One connection, so the second query must reuse the first one's.
	pool, err := postgres.OpenAdmin(ctx, d.dsn("reader", readerPassword), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var pid1, pid2 int
	err = postgres.InTenant(ctx, pool, d.a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid1)
	})
	if err != nil {
		t.Fatalf("InTenant: %v", err)
	}
	var setting *string
	if err := pool.QueryRow(ctx, "SELECT pg_backend_pid(), current_setting('app.tenant_id', true)").Scan(&pid2, &setting); err != nil {
		t.Fatal(err)
	}
	if pid1 != pid2 {
		t.Fatalf("the second query ran on backend %d, not %d; the test needs one connection", pid2, pid1)
	}
	// '' and not NULL: the policies' nullif exists for this.
	if setting == nil || *setting != "" {
		t.Errorf("app.tenant_id after the transaction = %v, want ''", setting)
	}
	tenants, keys := visible(t, pool)
	if len(tenants) != 0 || len(keys) != 0 {
		t.Errorf("after the tenant transaction the connection sees tenants %v and keys of %v, want nothing", tenants, keys)
	}
}

func TestRowLevelSecurityAppliesToTheTableOwnersPrivileges(t *testing.T) {
	t.Parallel()
	d := newTenantsDB(t)
	// A member of the owner has the owner's privileges but not its BYPASSRLS attribute.
	if _, err := openPool(t, d.Superuser).Exec(t.Context(), `
		CREATE ROLE owner_member LOGIN PASSWORD 'member-test' IN ROLE gateway_migrator;
		GRANT CONNECT ON DATABASE gateway TO owner_member;
	`); err != nil {
		t.Fatal(err)
	}
	tenants, keys := visible(t, openPool(t, d.dsn("owner_member", "member-test")))
	if len(tenants) != 0 || len(keys) != 0 {
		t.Errorf("a member of the owner sees tenants %v and keys of %v, want nothing", tenants, keys)
	}
}

func TestAppRoleCannotReadTenantsOrKeys(t *testing.T) {
	t.Parallel()
	d := newTenantsDB(t)
	app := openGatewayPool(t, d.App)
	for _, table := range []string{"tenants", "api_keys"} {
		if _, err := app.Exec(t.Context(), "SELECT * FROM "+table); !isPermissionDenied(err) {
			t.Errorf("app SELECT on %s = %v, want permission denied", table, err)
		}
	}
}

func TestAdminRoleCannotDeleteTenantsOrKeys(t *testing.T) {
	t.Parallel()
	d := newTenantsDB(t)
	for _, table := range []string{"api_keys", "tenants"} {
		if _, err := d.admin.Pool.Exec(t.Context(), "DELETE FROM "+table); !isPermissionDenied(err) {
			t.Errorf("admin DELETE on %s = %v, want permission denied", table, err)
		}
	}
}

func TestLookupFindsOnlyActiveKeys(t *testing.T) {
	t.Parallel()
	d := newTenantsDB(t)
	ctx := t.Context()
	keys := postgres.Keys{Pool: openGatewayPool(t, d.App)}

	id, err := keys.Lookup(ctx, tenant.HashKey(d.keyA))
	if err != nil || id != (tenant.Identity{TenantID: d.a, KeyID: d.idA}) {
		t.Fatalf("Lookup(key a) = %+v, %v; want tenant %s key %s", id, err, d.a, d.idA)
	}
	unknown, err := tenant.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Lookup(ctx, tenant.HashKey(unknown)); !errors.Is(err, tenant.ErrNoActiveKey) {
		t.Errorf("Lookup(unknown) = %v, want ErrNoActiveKey", err)
	}

	if err := d.admin.RevokeKey(ctx, d.idA); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Lookup(ctx, tenant.HashKey(d.keyA)); !errors.Is(err, tenant.ErrNoActiveKey) {
		t.Errorf("Lookup(revoked key) = %v, want ErrNoActiveKey", err)
	}

	if _, err := keys.Lookup(ctx, tenant.HashKey(d.keyB)); err != nil {
		t.Fatalf("Lookup(key b) before the disable: %v", err)
	}
	if err := d.admin.Disable(ctx, d.b); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Lookup(ctx, tenant.HashKey(d.keyB)); !errors.Is(err, tenant.ErrNoActiveKey) {
		t.Errorf("Lookup(key of a disabled tenant) = %v, want ErrNoActiveKey", err)
	}
}

func TestOnlyTheAppRoleCanCallTheLookup(t *testing.T) {
	t.Parallel()
	d := newTenantsDB(t)
	for _, dsn := range []struct{ name, user, password string }{
		{"admin", "gateway_admin", adminPassword},
		{"reader", "reader", readerPassword},
	} {
		_, err := postgres.Keys{Pool: openPool(t, d.dsn(dsn.user, dsn.password))}.Lookup(t.Context(), tenant.HashKey(d.keyA))
		if !isPermissionDenied(err) {
			t.Errorf("%s calling auth_lookup = %v, want permission denied", dsn.name, err)
		}
	}
}

func TestCreateKeyRefusesADisabledTenant(t *testing.T) {
	t.Parallel()
	d := newTenantsDB(t)
	ctx := t.Context()
	if err := d.admin.Disable(ctx, d.b); err != nil {
		t.Fatal(err)
	}
	_, key, err := d.admin.CreateKey(ctx, d.b)
	if !errors.Is(err, postgres.ErrTenantDisabled) || key != "" {
		t.Errorf("CreateKey(disabled tenant) = key %q, %v; want no key and ErrTenantDisabled", key, err)
	}
	if err == nil || !strings.Contains(err.Error(), "tenant "+d.b+" is disabled; a key for it could never authenticate") {
		t.Errorf("CreateKey(disabled tenant) error = %v, want it to name the tenant", err)
	}
	if keys, err := d.admin.ListKeys(ctx, d.b); err != nil || len(keys) != 1 || keys[0].ID != d.idB {
		t.Errorf("keys of the disabled tenant = %+v, %v; want only the key it had before", keys, err)
	}
	if _, _, err := d.admin.CreateKey(ctx, d.a); err != nil {
		t.Errorf("CreateKey(enabled tenant) = %v", err)
	}
}

// A disable that is still in its transaction must not let a key through:
// CreateKey waits for it, then sees disabled_at.
func TestCreateKeyWaitsForADisableInProgress(t *testing.T) {
	t.Parallel()
	d := newTenantsDB(t)
	ctx := t.Context()
	tx, err := d.admin.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "UPDATE tenants SET disabled_at = now() WHERE id = $1", d.b); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := d.admin.CreateKey(ctx, d.b)
		done <- err
	}()
	super := openPool(t, d.Superuser)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var waiting bool
		if err := super.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND usename = 'gateway_admin')").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("CreateKey returned %v while the disable was still open; it must wait for the tenant row", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("CreateKey never waited on the tenant row")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, postgres.ErrTenantDisabled) {
		t.Errorf("CreateKey after the disable committed = %v, want ErrTenantDisabled", err)
	}
}

func TestTenantAdminCommands(t *testing.T) {
	t.Parallel()
	d := newTenantsDB(t)
	ctx := t.Context()
	const missing = "00000000-0000-7000-8000-000000000000"

	if _, err := d.admin.Create(ctx, "tenant-a"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Create with a taken name = %v, want already exists", err)
	}
	if err := d.admin.Disable(ctx, missing); !errors.Is(err, postgres.ErrTenantNotFound) {
		t.Errorf("Disable(missing) = %v, want ErrTenantNotFound", err)
	}
	if _, _, err := d.admin.CreateKey(ctx, missing); !errors.Is(err, postgres.ErrTenantNotFound) {
		t.Errorf("CreateKey(missing tenant) = %v, want ErrTenantNotFound", err)
	}
	if err := d.admin.RevokeKey(ctx, missing); !errors.Is(err, postgres.ErrKeyNotFound) {
		t.Errorf("RevokeKey(missing) = %v, want ErrKeyNotFound", err)
	}

	all, err := d.admin.List(ctx)
	if err != nil || len(all) != 2 || all[0].ID != d.a || all[0].Name != "tenant-a" || all[0].DisabledAt != nil {
		t.Fatalf("List = %+v, %v", all, err)
	}
	if err := d.admin.Disable(ctx, d.a); err != nil {
		t.Fatal(err)
	}
	first, _ := d.admin.List(ctx)
	if err := d.admin.Disable(ctx, d.a); err != nil {
		t.Fatalf("a second Disable: %v", err)
	}
	second, _ := d.admin.List(ctx)
	if first[0].DisabledAt == nil || !first[0].DisabledAt.Equal(*second[0].DisabledAt) {
		t.Errorf("disabled_at = %v then %v, want the first time kept", first[0].DisabledAt, second[0].DisabledAt)
	}

	keys, err := d.admin.ListKeys(ctx, d.a)
	if err != nil || len(keys) != 1 || keys[0].ID != d.idA || keys[0].Prefix != tenant.DisplayPrefix(d.keyA) {
		t.Errorf("ListKeys(a) = %+v, %v", keys, err)
	}
}
