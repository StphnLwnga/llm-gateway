package postgres_test

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/httpapi"
	"github.com/StphnLwnga/llm-gateway/internal/store/postgres"
)

// postgresImage is the same image as deploy/compose.yaml.
const postgresImage = "pgvector/pgvector:0.8.7-pg18-trixie"

var discard = slog.New(slog.DiscardHandler)

// startPostgres skips without Docker locally, but fails in CI so the tests never silently stop.
func startPostgres(t *testing.T) (*tcpostgres.PostgresContainer, config.Secret) {
	t.Helper()
	ctx := t.Context()
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err == nil {
		err = provider.Health(ctx)
	}
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("Docker is required in CI for integration tests: %v", err)
		}
		t.Skipf("Docker is not available, skipping the Postgres integration test: %v", err)
	}

	ctr, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("gateway"),
		tcpostgres.WithUsername("gateway"),
		tcpostgres.WithPassword("gateway-test"),
		tcpostgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return ctr, config.Secret(dsn)
}

func openPool(t *testing.T, dsn config.Secret) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.Open(t.Context(), dsn, 4)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// appliedCount returns how many times goose recorded version as applied.
func appliedCount(t *testing.T, pool *pgxpool.Pool, version int64) int {
	t.Helper()
	var n int
	err := pool.QueryRow(t.Context(),
		"SELECT count(*) FROM goose_db_version WHERE version_id = $1 AND is_applied", version).Scan(&n)
	if err != nil {
		t.Fatalf("count applied: %v", err)
	}
	return n
}

func hasVectorExtension(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')").Scan(&ok); err != nil {
		t.Fatalf("query extensions: %v", err)
	}
	return ok
}

func TestMigrateUpStatusDownUp(t *testing.T) {
	t.Parallel()
	_, dsn := startPostgres(t)
	ctx := t.Context()
	pool := openPool(t, dsn)

	if err := postgres.Migrate(ctx, discard, dsn, "up"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if n := appliedCount(t, pool, 1); n != 1 {
		t.Fatalf("version 1 applied %d times, want 1", n)
	}
	if !hasVectorExtension(t, pool) {
		t.Error("the vector extension is missing after migrate up")
	}

	var status strings.Builder
	if err := postgres.Migrate(ctx, slog.New(slog.NewTextHandler(&status, nil)), dsn, "status"); err != nil {
		t.Fatalf("migrate status: %v", err)
	}
	if !strings.Contains(status.String(), "version=1") || !strings.Contains(status.String(), "state=applied") {
		t.Errorf("status does not report version 1 as applied:\n%s", status.String())
	}

	if err := postgres.Migrate(ctx, discard, dsn, "down"); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if hasVectorExtension(t, pool) {
		t.Error("the vector extension is still there after migrate down")
	}
	if err := postgres.Migrate(ctx, discard, dsn, "up"); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if !hasVectorExtension(t, pool) {
		t.Error("the vector extension is missing after down then up")
	}
}

func TestConcurrentMigrateUpAppliesOnce(t *testing.T) {
	t.Parallel()
	_, dsn := startPostgres(t)
	ctx := t.Context()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { errs[i] = postgres.Migrate(ctx, discard, dsn, "up") })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("migrate up #%d: %v", i+1, err)
		}
	}
	if n := appliedCount(t, openPool(t, dsn), 1); n != 1 {
		t.Errorf("version 1 applied %d times, want exactly 1", n)
	}
}

// TestMigrateLockSerializesOverlappingRuns exists because the embedded migration is too
// fast to overlap: without the lock, the test above still passes.
func TestMigrateLockSerializesOverlappingRuns(t *testing.T) {
	t.Parallel()
	_, dsn := startPostgres(t)
	ctx := t.Context()
	slow := fstest.MapFS{"00001_slow.sql": {Data: []byte(`-- +goose Up
CREATE TABLE IF NOT EXISTS applied_runs (at timestamptz NOT NULL DEFAULT now());
INSERT INTO applied_runs DEFAULT VALUES;
SELECT pg_sleep(1);

-- +goose Down
DROP TABLE applied_runs;
`)}}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { errs[i] = postgres.MigrateFiles(ctx, discard, dsn, "up", slow) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("migrate up #%d: %v", i+1, err)
		}
	}
	var runs int
	if err := openPool(t, dsn).QueryRow(ctx, "SELECT count(*) FROM applied_runs").Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Errorf("the migration body ran %d times, want exactly 1", runs)
	}
}

func TestMigrateRejectsAnUnknownCommand(t *testing.T) {
	t.Parallel()
	err := postgres.Migrate(t.Context(), discard, "postgres://unused", "sideways")
	if err == nil || !strings.Contains(err.Error(), "sideways") {
		t.Errorf("Migrate(sideways) = %v, want an unknown command error", err)
	}
}

func TestReadyzFollowsPostgres(t *testing.T) {
	t.Parallel()
	ctr, dsn := startPostgres(t)
	api := httpapi.New(discard, postgres.Pinger{Pool: openPool(t, dsn)})

	readyz := func() int {
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
		return rec.Code
	}
	if got := readyz(); got != http.StatusOK {
		t.Fatalf("GET /readyz with Postgres up = %d, want 200", got)
	}

	stopTimeout := 5 * time.Second
	if err := ctr.Stop(context.WithoutCancel(t.Context()), &stopTimeout); err != nil {
		t.Fatalf("stop postgres: %v", err)
	}
	start := time.Now()
	if got := readyz(); got != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz with Postgres stopped = %d, want 503", got)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("GET /readyz took %v with Postgres stopped; the check timeout should bound it to about 2s", took)
	}
}

func TestOpenDoesNotPrintThePassword(t *testing.T) {
	t.Parallel()
	_, err := postgres.Open(t.Context(), "postgres://gw:hunter2@db:notaport/gateway", 4)
	if err == nil {
		t.Fatal("Open succeeded with an invalid port, want an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("Open's error printed the password: %v", err)
	}
}

func TestOpenRejectsAPoolSizeOutsideInt32(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, math.MaxInt32 + 1, 1<<32 + 1} {
		if pool, err := postgres.Open(t.Context(), "postgres://gw:pw@127.0.0.1:1/gateway", n); err == nil {
			pool.Close()
			t.Errorf("Open with %d connections succeeded, want an error", n)
		}
	}
}

func TestOpenDoesNotDialPostgres(t *testing.T) {
	t.Parallel()
	// Nothing listens on port 1.
	pool, err := postgres.Open(t.Context(), "postgres://gw:pw@127.0.0.1:1/gateway?connect_timeout=1", 4)
	if err != nil {
		t.Fatalf("Open with Postgres down: %v, want a pool", err)
	}
	defer pool.Close()
	if err := (postgres.Pinger{Pool: pool}).Check(t.Context()); err == nil {
		t.Error("Check succeeded with Postgres down, want an error")
	}
}
