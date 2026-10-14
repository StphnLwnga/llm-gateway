package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/StphnLwnga/llm-gateway/internal/config"
)

// syncBuffer lets run's logger and the test share one buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// buildGatewayctl builds the real admin binary, so the test drives it the way an operator does.
func buildGatewayctl(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gatewayctl")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "github.com/StphnLwnga/llm-gateway/cmd/gatewayctl")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build gatewayctl: %v\n%s", err, out)
	}
	return bin
}

func TestRoutesAddedWithGatewayctlAppearInModels(t *testing.T) {
	t.Parallel()
	dsns := startPostgres(t)
	ctl := buildGatewayctl(t)
	gatewayctl := func(dsn config.Secret, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), ctl, args...)
		cmd.Env = append(os.Environ(), "DATABASE_URL="+string(dsn), "LOG_FORMAT=text")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gatewayctl %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	var key string // set once gatewayctl has created it

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	ctx, cancel := context.WithCancel(t.Context())
	var logs syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, env(map[string]string{
			"PORT": port, "DATABASE_URL": string(dsns.App), "ROUTES_REFRESH_INTERVAL": "200ms",
			"OLLAMA_BASE_URL": "http://127.0.0.1:1", // enabled; no request reaches it in this test
		}), &logs)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	get := func(path string) (int, []byte) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatalf("timed out waiting for %s\nlogs:\n%s", what, logs.String())
	}
	// aliases returns the listed model IDs, and false when the request failed.
	aliases := func() ([]string, bool) {
		code, body := get("/v1/models")
		if code != http.StatusOK {
			return nil, false
		}
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &list)
		out := []string{}
		for _, m := range list.Data {
			out = append(out, m.ID)
		}
		return out, true
	}

	// The routes table does not exist yet, so no snapshot has loaded.
	waitFor("the gateway to listen", func() bool { code, _ := get("/healthz"); return code == http.StatusOK })
	if code, _ := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before the routes loaded = %d, want 503", code)
	}

	gatewayctl(dsns.Migrator, "migrate", "up")
	waitFor("/readyz to be 200 after the first load", func() bool { code, _ := get("/readyz"); return code == http.StatusOK })
	if code, _ := get("/v1/models"); code != http.StatusUnauthorized {
		t.Fatalf("/v1/models with no key = %d, want 401", code)
	}

	tenantID := strings.Fields(gatewayctl(dsns.Admin, "tenants", "create", "--name", "e2e"))[1]
	created := strings.Split(gatewayctl(dsns.Admin, "keys", "create", "--tenant", tenantID), "\n")
	keyID := strings.Fields(created[0])[1]
	key = created[1]

	gatewayctl(dsns.Admin, "routes", "add", "--alias", "local-chat", "--provider", "ollama", "--upstream-model", "llama3.2:3b", "--priority", "1")
	gatewayctl(dsns.Admin, "routes", "add", "--alias", "gpt", "--provider", "openai", "--upstream-model", "gpt-x", "--priority", "1")
	waitFor("local-chat in /v1/models", func() bool { a, ok := aliases(); return ok && slices.Equal(a, []string{"local-chat"}) })

	gatewayctl(dsns.Admin, "routes", "disable", "1")
	waitFor("local-chat to leave /v1/models", func() bool { a, ok := aliases(); return ok && len(a) == 0 })

	gatewayctl(dsns.Admin, "keys", "revoke", keyID)
	if code, _ := get("/v1/models"); code != http.StatusUnauthorized {
		t.Errorf("/v1/models with a revoked key = %d, want 401", code)
	}
	if strings.Contains(logs.String(), key) {
		t.Error("the gateway logged the key")
	}
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
