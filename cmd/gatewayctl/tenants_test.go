package main

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/StphnLwnga/llm-gateway/internal/config"
	"github.com/StphnLwnga/llm-gateway/internal/store/postgres"
	"github.com/StphnLwnga/llm-gateway/internal/tenant"
)

func TestTenantAndKeyCommandsAgainstPostgres(t *testing.T) {
	t.Parallel()
	dsns := startPostgres(t)
	ctlAs := func(dsn config.Secret, args ...string) (string, error) {
		var out bytes.Buffer
		err := run(t.Context(), args, env(map[string]string{"DATABASE_URL": string(dsn), "LOG_FORMAT": "text"}), &out, io.Discard)
		return out.String(), err
	}
	ctl := func(args ...string) string {
		t.Helper()
		out, err := ctlAs(dsns.Admin, args...)
		if err != nil {
			t.Fatalf("gatewayctl %v: %v", args, err)
		}
		return out
	}

	if _, err := ctlAs(dsns.Admin, "migrate", "up"); err == nil || !strings.Contains(err.Error(), "migrate needs the migrator role; use the migrate service") {
		t.Fatalf("migrate up as the admin role = %v, want the migrator role message", err)
	}
	if _, err := ctlAs(dsns.Migrator, "migrate", "up"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	created := regexp.MustCompile(`^tenant (\S+) created: acme\n$`).FindStringSubmatch(ctl("tenants", "create", "--name", "acme"))
	if created == nil {
		t.Fatal("tenants create printed an unexpected line")
	}
	tenantID := created[1]

	out := ctl("keys", "create", "--tenant", tenantID)
	m := regexp.MustCompile(`^key (\S+) created for tenant (\S+), prefix (\S+)\n(\S+)\nThe key is not shown again. Store it now.\n$`).FindStringSubmatch(out)
	if m == nil || m[2] != tenantID {
		t.Fatalf("keys create printed:\n%s", out)
	}
	keyID, prefix, key := m[1], m[3], m[4]
	if err := tenant.ParseKey(key); err != nil || tenant.DisplayPrefix(key) != prefix {
		t.Errorf("keys create printed key %q with prefix %q: %v", key, prefix, err)
	}

	// keyRow returns the key's REVOKED and LAST USED columns.
	keyRow := func() (revoked, lastUsed string) {
		t.Helper()
		list := ctl("keys", "list", "--tenant", tenantID)
		lines := strings.Split(strings.TrimSpace(list), "\n")
		if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "ID PREFIX CREATED REVOKED LAST USED" {
			t.Fatalf("keys list:\n%s", list)
		}
		row := strings.Fields(lines[1])
		if len(row) != 5 || row[0] != keyID || row[1] != prefix || strings.Contains(list, key) {
			t.Fatalf("keys list must show the ID and prefix but never the key:\n%s", list)
		}
		return row[3], row[4]
	}
	if revoked, lastUsed := keyRow(); revoked != "-" || lastUsed != "-" {
		t.Errorf("a new key: revoked %q, last used %q; want - and -", revoked, lastUsed)
	}
	app, err := postgres.Open(t.Context(), dsns.App, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := (postgres.Keys{Pool: app}).Touch(t.Context(), []string{keyID}); err != nil {
		t.Fatalf("touch the key as the gateway: %v", err)
	}
	if _, lastUsed := keyRow(); lastUsed == "-" {
		t.Error("keys list after the gateway used the key shows no last used time")
	}
	if out := ctl("keys", "revoke", keyID); out != "key "+keyID+" revoked\n" {
		t.Errorf("keys revoke printed %q", out)
	}
	if revoked, _ := keyRow(); revoked == "-" {
		t.Error("keys list after revoke shows no revoke time")
	}

	if out := ctl("tenants", "disable", tenantID); out != "tenant "+tenantID+" disabled\n" {
		t.Errorf("tenants disable printed %q", out)
	}
	out, err = ctlAs(dsns.Admin, "keys", "create", "--tenant", tenantID)
	if want := "tenant " + tenantID + " is disabled; a key for it could never authenticate"; err == nil || !strings.Contains(err.Error(), want) || exitCode(err) != 1 || out != "" {
		t.Errorf("keys create for a disabled tenant = %q, %v; want no output, exit 1 and %q", out, err, want)
	}
	tenants := ctl("tenants", "list")
	if lines := strings.Split(strings.TrimSpace(tenants), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[1], tenantID+"  acme") || strings.HasSuffix(lines[1], "-") {
		t.Errorf("tenants list after disable:\n%s", tenants)
	}

	if _, err := ctlAs(dsns.Admin, "keys", "revoke", "00000000-0000-7000-8000-000000000000"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("keys revoke on a missing key = %v, want not found", err)
	}
}
