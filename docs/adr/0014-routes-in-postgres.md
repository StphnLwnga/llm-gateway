# 0014. Routes in a Postgres table, served from an in-memory snapshot

- Status: Accepted
- Date: 2026-10-11
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 1: a8b65dd, 9f677e8, 6797f63, 6122f06, 7a2671a, 5e4674b. Milestone 4 (planned): failover and breakers on top of the same snapshot.

## Context

A route maps a model alias, the `model` value a client sends, to a provider and an upstream model, with a priority for failover and prices for cost reporting. Every request resolves its alias, so the lookup is on the hot path. Operators add, disable and reprice routes while the gateway runs, and every replica must serve the same routes. Milestone 3 adds per-tenant routes, and milestone 6 records the price charged for each request.

## Considered options

1. A Postgres table. Each replica loads the enabled routes into memory and reloads them on an interval.
2. A config file (YAML) in the image or a ConfigMap, read at startup or on change.
3. A Postgres table, queried on every request.

## Pros and cons

| Option | Pros | Cons |
|---|---|---|
| Table plus in-memory snapshot | No database round trip on the request path. A change needs no deploy. Constraints in the table (provider set, non-negative prices, unique alias and priority) hold for every writer. Per-tenant routes are an added column. | A change reaches each replica within one refresh interval, so replicas can briefly disagree. The table has no history of who changed what; that needs an audit table. |
| Config file | Every change is a reviewed commit, so the history and the author are free. No database dependency for routing. | A change needs a deploy or a ConfigMap rollout. Validation happens only when the file loads. Per-tenant routes for many tenants make the file large and hard to review. |
| Table, queried per request | Changes apply at once. | A database round trip on every request, and the request path fails when Postgres is slow or down. |

## Decision outcome

Chosen: option 1.

```text
gatewayctl routes add ──> Postgres routes table
                              │  every ROUTES_REFRESH_INTERVAL (default 10s)
                              v
each replica: in-memory snapshot (atomic swap) ──> Resolve(alias), GET /v1/models
```

- **The snapshot.** `internal/router` loads every enabled route, groups the routes by alias, sorts each group by priority, and swaps the whole snapshot with `atomic.Pointer`. A reader always sees one complete snapshot. `Resolve` returns a copy, so no caller can change the shared one.
- **A failed reload keeps the last good snapshot** and logs a warning. Postgres being down for a moment does not take routing down.
- **Readiness.** `/readyz` returns 503 until the first snapshot loads, so a new pod gets no traffic before it can route. After that, a failed reload does not make the pod unready: every pod shares the same Postgres, so failing readiness on a reload error would take all pods out of rotation at once while they can still serve from memory.
- **Routes without an enabled provider** (no key, or no `OLLAMA_BASE_URL`) are left out of the snapshot. The router logs them once each time that set changes, not on every reload.
- **Exact prices.** Prices are USD per million tokens, stored as `numeric(14, 6)` and handled in Go as integer micro-USD (`money.MicroUSD`). Postgres converts between them in SQL, so no float is ever involved. An integer type was chosen over a decimal library or `pgtype.Numeric` because sums and per-token costs stay exact with plain integer arithmetic, and six decimal places are enough for any published per-million price.
- **Polling, not LISTEN/NOTIFY.** A trigger with `NOTIFY` and a `LISTEN` per replica would apply a change in well under a second. It needs a dedicated connection per replica outside the pool, reconnect handling, and a full reload after each reconnect, because notifications sent while disconnected are lost. It also does not work through PgBouncer in transaction pooling mode, which lists `LISTEN` as "Never", and a pooler is an option for scale-out ([design.md](../design.md), section 8). One query per interval per replica costs little at this scale.

## Consequences

- Good: the request path never waits on Postgres for routing.
- Good: `gatewayctl routes` changes routes on a running gateway, within one interval.
- Bad: for up to one interval after a change, replicas can route the same alias differently.
- Bad: there is no audit trail beyond `updated_at`. A history of route and price changes needs an audit table or trigger before routes are changed by more than one operator.
- Bad: a price change applies to later requests only, so the ledger (milestone 6) must store the price charged with each request, not look it up later.

## Links

- PostgreSQL, `NOTIFY` (delivery on commit, payload limit): https://www.postgresql.org/docs/current/sql-notify.html
- PostgreSQL, `LISTEN`: https://www.postgresql.org/docs/current/sql-listen.html
- PgBouncer, SQL feature map for pooling modes: https://www.pgbouncer.org/features.html
- PostgreSQL, numeric types: https://www.postgresql.org/docs/current/datatype-numeric.html
- `sync/atomic.Pointer`: https://pkg.go.dev/sync/atomic#Pointer
- Code: `internal/router/router.go`, `internal/store/postgres/routes.go`, `internal/store/postgres/migrations/00002_routes.sql`, `internal/money/money.go`, `cmd/gatewayctl/main.go`
