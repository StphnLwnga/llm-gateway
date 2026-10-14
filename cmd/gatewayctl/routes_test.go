package main

import (
	"bytes"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/StphnLwnga/llm-gateway/internal/config"
)

func TestRoutesCommandsAgainstPostgres(t *testing.T) {
	t.Parallel()
	dsns := startPostgres(t)
	ctlAs := func(dsn config.Secret, args ...string) (string, error) {
		var out bytes.Buffer
		err := run(t.Context(), args, env(map[string]string{"DATABASE_URL": string(dsn), "LOG_FORMAT": "text"}), &out, io.Discard)
		return out.String(), err
	}
	ctl := func(args ...string) (string, error) { return ctlAs(dsns.Admin, args...) }

	if _, err := ctlAs(dsns.Migrator, "migrate", "up"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	out, err := ctl("routes", "add", "--alias", "chat", "--provider", "groq", "--upstream-model", "llama-8b",
		"--priority", "10", "--input-price", "0.05", "--output-price", "0.08", "--default-max-tokens", "256", "--max-max-tokens", "4096")
	if err != nil || !strings.Contains(out, "route 1 added: chat -> groq/llama-8b, priority 10") {
		t.Fatalf("routes add = %q, %v", out, err)
	}
	if _, err := ctl("routes", "add", "--alias", "chat", "--provider", "ollama", "--upstream-model", "llama3.2:3b", "--priority", "20", "--disabled"); err != nil {
		t.Fatalf("routes add second: %v", err)
	}
	if _, err := ctl("routes", "add", "--alias", "chat", "--provider", "openai", "--upstream-model", "x", "--priority", "10"); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Errorf("a second route at the same priority = %v, want a conflict", err)
	}

	out, err = ctl("routes", "list")
	if err != nil {
		t.Fatalf("routes list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") {
		t.Fatalf("routes list printed:\n%s", out)
	}
	for _, want := range [][]string{
		{"1", "chat", "10", "groq", "llama-8b", "true", "0.050000", "0.080000", "256", "4096"},
		{"2", "chat", "20", "ollama", "llama3.2:3b", "false", "0.000000", "0.000000", "-", "-"},
	} {
		if got := strings.Fields(lines[1+indexOf(want[0], lines[1:])]); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("row = %q, want %q", got, want)
		}
	}

	if out, err := ctl("routes", "enable", "2"); err != nil || !strings.Contains(out, "route 2 enabled") {
		t.Errorf("routes enable 2 = %q, %v", out, err)
	}
	if out, err := ctl("routes", "disable", "1"); err != nil || !strings.Contains(out, "route 1 disabled") {
		t.Errorf("routes disable 1 = %q, %v", out, err)
	}
	if _, err := ctl("routes", "enable", "99"); err == nil || !strings.Contains(err.Error(), "route not found") {
		t.Errorf("routes enable 99 = %v, want route not found", err)
	}
}

func indexOf(id string, rows []string) int {
	for i, r := range rows {
		if f := strings.Fields(r); len(f) > 0 && f[0] == id {
			return i
		}
	}
	return 0
}

// postgresImage is the same image as deploy/compose.yaml. Test files hold
// their own copy of this helper because a shared non-test package would pull
// testcontainers into the code that govulncheck scans.
const postgresImage = "pgvector/pgvector:0.8.7-pg18-trixie"

// roleDSNs holds a connection string per role that deploy/postgres/initdb/10-roles.sh creates.
type roleDSNs struct {
	Migrator, App, Admin config.Secret
}

// startPostgres skips without Docker locally, but fails in CI so the tests never silently stop.
func startPostgres(t *testing.T) roleDSNs {
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

	passwords := map[string]string{"MIGRATOR_PASSWORD": "migrator-test", "APP_PASSWORD": "app-test", "ADMIN_PASSWORD": "admin-test"}
	ctr, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("gateway"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres-test"),
		tcpostgres.WithInitScripts("../../deploy/postgres/initdb/10-roles.sh"),
		testcontainers.WithEnv(passwords),
		tcpostgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	addr, err := ctr.PortEndpoint(ctx, "5432/tcp", "")
	if err != nil {
		t.Fatalf("postgres address: %v", err)
	}
	dsn := func(user, password string) config.Secret {
		u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: addr, Path: "/gateway", RawQuery: "sslmode=disable"}
		return config.Secret(u.String())
	}
	return roleDSNs{
		Migrator: dsn("gateway_migrator", passwords["MIGRATOR_PASSWORD"]),
		App:      dsn("gateway_app", passwords["APP_PASSWORD"]),
		Admin:    dsn("gateway_admin", passwords["ADMIN_PASSWORD"]),
	}
}
