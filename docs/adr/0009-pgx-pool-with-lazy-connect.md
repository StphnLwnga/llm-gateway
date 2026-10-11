# 0009. pgx pool with lazy connect

- Status: Accepted
- Date: 2026-10-10
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 0: db6bd57, and the pool size bounds in e7b7e0e and 8829e5f.

## Context

The gateway needs a Postgres connection pool for the request path, and a readiness check that reports whether the pod can use the database. Two questions:

1. Which driver and pool.
2. Whether the gateway connects at startup or on first use.

Postgres has a hard connection limit (`max_connections`, 100 by default), shared by every process that connects. Each gateway replica opens up to `DB_MAX_CONNS` connections, so `replicas x DB_MAX_CONNS` plus the migration step and admin headroom must stay below that limit.

## Considered options

1. pgx v5 `pgxpool`, lazy connect: the pool is created without dialing, and the first query or readiness ping opens the first connection.
2. pgx v5 `pgxpool`, eager connect: ping at startup and exit if Postgres is down.
3. `database/sql` with pgx's `stdlib` driver.

## Pros and cons

| Option | Pros | Cons |
|---|---|---|
| pgxpool, lazy | The gateway starts while Postgres is down, so start order does not matter. A Postgres outage shows as `/readyz` 503, not as a crash loop. Native pgx types and protocol features. | A configuration error in the host or password shows at the first ping, not at startup. |
| pgxpool, eager | A wrong connection string fails at startup. | A Postgres outage during a deploy or restart makes every new pod exit, and Kubernetes restarts them in a loop. |
| `database/sql` and pgx `stdlib` | The standard interface, which many tools accept. | Loses pgx-specific features on the request path (native types, batch, copy). |

## Decision outcome

Chosen: option 1. `postgres.Open` parses `DATABASE_URL` and returns a pool without dialing.

- **Pool size.** `DB_MAX_CONNS` defaults to 10 and accepts 1 to 1000 (config). `Open` also rejects a size outside 1 to `math.MaxInt32` before it converts to `int32`, because `Open` is exported and any caller can pass any `int`. Without that check, 4294967297 converts to 1 and the pool would silently have one connection. The check also clears gosec G115.
- **Readiness.** `postgres.Pinger` pings through the shared pool, with the 2 second timeout that `/readyz` gives each check.
- **Secrets.** A parse error from pgx can quote the connection string, so `Open` returns a fixed message instead.
- `gatewayctl` uses pgx through `database/sql` only for goose ([0010](0010-migrations-with-goose.md)), which needs that interface.

Measured on Compose: after Postgres stopped, `/readyz` returned 503 within 320 ms, `/healthz` stayed 200, and the gateway did not restart. When Postgres came back, `/readyz` returned 200 with no action on the gateway.

## Consequences

- Good: a database outage takes pods out of rotation and never restarts them.
- Bad (known trade-off): `pgxpool`'s `Ping` borrows a connection from the same pool that serves requests. When every connection is busy, the ping waits, the 2 second check times out, and `/readyz` returns 503 although Postgres is healthy. Under heavy load every pod could fail readiness at once. The current deployment has no load that can exhaust a pool. Milestone 8 runs the HPA under k6 load and decides with measurements.
- Bad: scale-out multiplies connections. Ten replicas with a pool of 10 reach the default limit of 100. Milestone 8 sizes pools per replica or adds a connection pooler such as PgBouncer.

The readiness options milestone 8 compares:

| Readiness check | What `/readyz` then means | Risk |
|---|---|---|
| Ping through the shared pool (current) | This pod can get a database connection now | All pods can fail readiness together under load |
| Ping on a dedicated connection | Postgres is up | A pod with an exhausted pool stays in rotation |
| No database check | Only that the pod is not draining | A pod with no database keeps getting traffic |

## Links

- pgxpool: https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool
- PostgreSQL, `max_connections`: https://www.postgresql.org/docs/current/runtime-config-connection.html
- gosec rules (G115): https://github.com/securego/gosec/blob/master/RULES.md
- Code: `internal/store/postgres/postgres.go`, `internal/config/config.go`
