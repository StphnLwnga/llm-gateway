# 0003. Postgres as the system of record, Redis only for admission control

- Status: Accepted
- Date: 2026-10-10
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 0: the Postgres pool and migrations. Milestones 3 and 6 (planned): tenants, keys, credentials, ledger, outbox. Milestone 7 (planned): Redis. Milestone 11 (planned): pgvector.

## Context

The gateway keeps two kinds of state with different needs.

- **Durable records:** tenants, API keys, encrypted provider credentials, routes, budgets, the usage ledger, request logs and the outbox. A lost or duplicated ledger row is a billing error. These need transactions, constraints and backups.
- **Admission state on the request path:** rate-limit windows, budget holds and the exact-match cache. Every request reads and writes it before it reaches a provider, so it must be fast and atomic. It can be rebuilt.

## Considered options

1. Postgres for every durable record. Redis only for admission control: rate limits, budget holds, the exact cache.
2. Postgres only, with rate limits and holds in Postgres tables.
3. Redis as the primary store for spend, flushed to Postgres in batches.

## Pros and cons

| Option | Pros | Cons |
|---|---|---|
| Postgres records, Redis admission | Each store does what it is good at. The outbox row commits in the same transaction as the request's records (transactional outbox, M6). Redis Lua scripts make a check-and-reserve atomic. Redis state can be rebuilt from the Postgres ledger. | Two stores to run. The Redis spend can drift from the ledger, so a reconciler is needed. |
| Postgres only | One store. No drift. | Every request adds row-lock writes on hot rows (a tenant's limit window and budget). Under concurrency those rows serialize, and the pool fills. |
| Redis primary for spend | Fastest path. | A Redis failure between flushes loses spend that was already charged. The ledger is no longer the record of truth. |

## Decision outcome

Chosen: option 1.

- Postgres is the system of record. pgvector (enabled by migration 00001) keeps the semantic cache (M11) in the same database, under the same tenant isolation as the other tables.
- Redis holds only state that can be lost and rebuilt: sliding-window limits, budget holds with a TTL, and the exact cache keyed by an HMAC per tenant.
- A reconciler in `cmd/worker` rebuilds Redis spend from the Postgres ledger.

## Consequences

- Good: the ledger has one source, so billing and analytics agree.
- Good: losing Redis loses no record. What the client sees while Redis is down is decided in milestone 7 (fail open or fail closed, per check).
- Bad: the pool size per replica limits scale-out (`replicas x DB_MAX_CONNS` must stay below `max_connections`). See [0009](0009-pgx-pool-with-lazy-connect.md).
- Bad: Redis spend lags the ledger by the reconciler interval.

## Links

- PostgreSQL row security policies: https://www.postgresql.org/docs/current/ddl-rowsecurity.html
- pgvector: https://github.com/pgvector/pgvector
- Redis, scripting with Lua: https://redis.io/docs/latest/develop/programmability/eval-intro/
- Design: [docs/design.md](../design.md), sections 4 and 5
