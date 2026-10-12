package postgres_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/StphnLwnga/llm-gateway/internal/money"
	"github.com/StphnLwnga/llm-gateway/internal/router"
	"github.com/StphnLwnga/llm-gateway/internal/store/postgres"
)

func migratedRoutes(t *testing.T) (postgres.Routes, *pgxpool.Pool) {
	t.Helper()
	_, dsn := startPostgres(t)
	if err := postgres.Migrate(t.Context(), discard, dsn, "up"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	pool := openPool(t, dsn)
	return postgres.Routes{Pool: pool}, pool
}

func usd(t *testing.T, s string) money.MicroUSD {
	t.Helper()
	m, err := money.ParseUSD(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRoutesAddListEnableDisable(t *testing.T) {
	t.Parallel()
	routes, _ := migratedRoutes(t)
	ctx := t.Context()
	def, maxTokens := 256, 4096
	id, err := routes.Add(ctx, router.Route{
		Alias: "chat", Provider: "groq", UpstreamModel: "llama-8b", Priority: 10, Enabled: true,
		InputPrice: usd(t, "0.05"), OutputPrice: usd(t, "0.08"), DefaultMaxTokens: &def, MaxMaxTokens: &maxTokens,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := routes.Add(ctx, router.Route{Alias: "chat", Provider: "ollama", UpstreamModel: "llama3.2:3b", Priority: 20, Enabled: true}); err != nil {
		t.Fatalf("Add second: %v", err)
	}

	all, err := routes.List(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("List = %d routes, %v; want 2", len(all), err)
	}
	got := all[0]
	if got.ID != id || got.Alias != "chat" || got.Provider != "groq" || got.Priority != 10 || !got.Enabled ||
		got.InputPrice.String() != "0.050000" || got.OutputPrice.String() != "0.080000" ||
		*got.DefaultMaxTokens != 256 || *got.MaxMaxTokens != 4096 || got.CreatedAt.IsZero() {
		t.Errorf("first route = %+v", got)
	}
	if all[1].DefaultMaxTokens != nil || all[1].InputPrice != 0 {
		t.Errorf("second route = %+v, want no max tokens and a zero price", all[1])
	}

	if err := routes.SetEnabled(ctx, id, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	enabled, err := routes.EnabledRoutes(ctx)
	if err != nil || len(enabled) != 1 || enabled[0].Provider != "ollama" {
		t.Errorf("EnabledRoutes after disabling groq = %+v, %v", enabled, err)
	}
	if err := routes.SetEnabled(ctx, 9999, true); !errors.Is(err, postgres.ErrRouteNotFound) {
		t.Errorf("SetEnabled on a missing id = %v, want ErrRouteNotFound", err)
	}
}

func TestRoutePricesRoundTripExactly(t *testing.T) {
	t.Parallel()
	routes, _ := migratedRoutes(t)
	prices := []string{"0.000150", "0.000001", "0.05", "15", "99999999.999999"}
	for i, p := range prices {
		if _, err := routes.Add(t.Context(), router.Route{
			Alias: "price-test", Provider: "openai", UpstreamModel: "m" + p, Priority: i, Enabled: true,
			InputPrice: usd(t, p), OutputPrice: usd(t, p),
		}); err != nil {
			t.Fatalf("Add %s: %v", p, err)
		}
	}
	all, err := routes.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i, rt := range all {
		want := usd(t, prices[i])
		if rt.InputPrice != want || rt.OutputPrice != want {
			t.Errorf("price %s came back as %s and %s", prices[i], rt.InputPrice, rt.OutputPrice)
		}
	}
}

func TestRoutesRejectBadRows(t *testing.T) {
	t.Parallel()
	routes, pool := migratedRoutes(t)
	ctx := t.Context()

	if _, err := routes.Add(ctx, router.Route{Alias: "x", Provider: "anthropic", UpstreamModel: "m"}); err == nil || !strings.Contains(err.Error(), "provider") {
		t.Errorf("Add with an unknown provider = %v, want a provider error", err)
	}
	if _, err := routes.Add(ctx, router.Route{Alias: "x", Provider: "groq", UpstreamModel: "m", InputPrice: -1}); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("Add with a negative price = %v, want an error", err)
	}

	// The table's own constraints hold even when Validate is skipped.
	for name, sql := range map[string]string{
		"unknown provider":    `INSERT INTO routes (alias, provider, upstream_model, priority) VALUES ('x', 'anthropic', 'm', 1)`,
		"negative price":      `INSERT INTO routes (alias, provider, upstream_model, priority, input_usd_per_mtok) VALUES ('x', 'groq', 'm', 1, -0.01)`,
		"max below default":   `INSERT INTO routes (alias, provider, upstream_model, priority, default_max_tokens, max_max_tokens) VALUES ('x', 'groq', 'm', 1, 100, 10)`,
		"empty upstream":      `INSERT INTO routes (alias, provider, upstream_model, priority) VALUES ('x', 'groq', '', 1)`,
		"zero default tokens": `INSERT INTO routes (alias, provider, upstream_model, priority, default_max_tokens) VALUES ('x', 'groq', 'm', 1, 0)`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s: the table accepted it", name)
		}
	}

	if _, err := routes.Add(ctx, router.Route{Alias: "dup", Provider: "groq", UpstreamModel: "a", Priority: 1}); err != nil {
		t.Fatal(err)
	}
	for name, rt := range map[string]router.Route{
		"same alias and priority":        {Alias: "dup", Provider: "ollama", UpstreamModel: "b", Priority: 1},
		"same alias, provider and model": {Alias: "dup", Provider: "groq", UpstreamModel: "a", Priority: 2},
	} {
		if _, err := routes.Add(ctx, rt); err == nil || !strings.Contains(err.Error(), "conflicts") {
			t.Errorf("%s: Add = %v, want a conflict", name, err)
		}
	}
}
