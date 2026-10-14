# 0010. Migrations: goose, embedded, a one-shot step, a session advisory lock

- Status: Accepted
- Date: 2026-10-10
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 0: db6bd57, 2e41e28, 4a27ba3

## Context

The schema grows with almost every feature. Migrations must apply each change once, in order, on every environment, and must be safe when two runs start together: a Kubernetes Job can retry, and two deploys can overlap. The schema uses a Postgres extension (`vector`, migration 00001), and milestone 3 adds row-level security policies and separate database roles.

Two choices: the migration tool, and when migrations run.

## Considered options

Tool:

1. goose v3, with its `Provider` API and a Postgres session locker.
2. golang-migrate.
3. Atlas.

When they run:

1. A separate one-shot step, `gatewayctl migrate up`, before the gateway starts.
2. Inside the gateway at startup.

## Pros and cons

| Tool | Pros | Cons |
|---|---|---|
| goose v3 | Plain SQL files with `Up` and `Down` sections. A transaction per migration by default. The `Provider` API holds no global state, accepts an `fs.FS`, and takes a session locker that uses a Postgres advisory lock. | Down migrations are written by hand. |
| golang-migrate | Widely used, many database drivers. | A failed migration leaves the database "dirty", and its FAQ says you "need to manually fix the error and then force the expected version". Every failure then needs a manual step. |
| Atlas | Declarative schema as code, with migration planning and linting. | The Apache 2.0 Community Edition lacks support for several database objects, including extensions and row-level security, and lacks migration linting. Those are in the standard distribution, which is proprietary and needs a login for its Pro features. |

| When | Pros | Cons |
|---|---|---|
| One-shot step | Runs once per deploy, not once per replica. The gateway never serves against a schema older than its code: Compose starts the gateway only after `migrate` exits 0. A failed migration stops the deploy before any new pod serves. | One more step in Compose and Kubernetes. |
| At gateway startup | Nothing extra to run. | Every replica races to migrate on every start. A slow migration delays readiness for all pods. The gateway needs DDL privileges at runtime. |

## Decision outcome

Chosen: goose v3, run as a separate one-shot step.

- **Embedded.** `//go:embed migrations/*.sql` compiles the SQL into `gatewayctl`, so a binary always runs the schema it was built with, and the image needs no migrations folder.
- **One transaction per migration.** A failed migration leaves no partial change.
- **Session advisory lock.** goose takes a Postgres advisory lock on one connection and runs every migration on that connection. A second run waits for the lock, then finds nothing to apply. If `gatewayctl` crashes, the session ends and Postgres releases the lock. The retry is every 1 second, up to 120 times (goose's default is every 5 seconds for 5 minutes, so a waiting run would idle up to 5 seconds after the first run ends).
- **Proof.** `TestMigrateLockSerializesOverlappingRuns` gives `migrate` a test-only migration that sleeps for 1 second, so the two runs overlap. Without the lock, the test fails. `TestConcurrentMigrateUpAppliesOnce` alone does not prove the lock: the real migration is too fast for two runs to overlap.

```text
run A: lock ok ─> apply 00001 ─> unlock
run B: lock busy ─> retry 1s ... ─> lock ok ─> version 1 applied, nothing to do ─> unlock
```

## Consequences

- Good: concurrent runs are safe, and a crashed run never leaves a stuck lock.
- Closed in milestone 3: `migrate` runs as `gateway_migrator`, which owns the schema and runs DDL. The gateway runs as `gateway_app`, which owns no table, has no `BYPASSRLS`, and has only the rights each migration grants. Because table owners bypass row-level security unless the table uses `FORCE ROW LEVEL SECURITY`, the gateway checks its role on every new connection and refuses a superuser, a `BYPASSRLS` role, or a role that owns a table in schema `public`, directly or through a role it can become. `gatewayctl` admin commands run as `gateway_admin`, which has `BYPASSRLS` and only the rights each migration grants.
- Bad: each migration needs a hand-written `Down` section.

## Links

- goose: https://pkg.go.dev/github.com/pressly/goose/v3
- goose Provider and session locking: https://pressly.github.io/goose/blog/2023/goose-provider/
- PostgreSQL, advisory locks: https://www.postgresql.org/docs/current/explicit-locking.html#ADVISORY-LOCKS
- PostgreSQL, row security policies (who bypasses them): https://www.postgresql.org/docs/current/ddl-rowsecurity.html
- golang-migrate FAQ (dirty state): https://github.com/golang-migrate/migrate/blob/master/FAQ.md
- Atlas Community Edition: https://atlasgo.io/community-edition
- Docker Compose, `depends_on` conditions: https://docs.docker.com/reference/compose-file/services/#depends_on
- Code: `internal/store/postgres/migrate.go`, `cmd/gatewayctl/main.go`, `deploy/compose.yaml`
