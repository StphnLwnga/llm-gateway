// Package pgtest starts a throwaway Postgres for integration tests.
package pgtest

import (
	"os"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/StphnLwnga/llm-gateway/internal/config"
)

// Image is the same image as deploy/compose.yaml.
const Image = "pgvector/pgvector:0.8.7-pg18-trixie"

// Start runs Postgres in a container for the test. It skips without Docker
// locally, but fails in CI so the tests never silently stop running.
func Start(t *testing.T) (*tcpostgres.PostgresContainer, config.Secret) {
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

	ctr, err := tcpostgres.Run(ctx, Image,
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
