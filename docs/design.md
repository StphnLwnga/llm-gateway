# llm-gateway design

llm-gateway is a multi-tenant, OpenAI-compatible LLM gateway. Each tenant calls one API with its own key. The gateway authenticates the key, applies the tenant's rate limits and budget, routes the request to a provider (Ollama, Groq, OpenAI or Anthropic), streams the answer back, and records usage and cost.

This document is the target design. Each part is marked:

- **Built (Mn)**: in the code now, added in milestone n.
- **Planned (Mn)**: designed here, built in milestone n.

Decisions are recorded in [docs/adr/](adr/README.md). Reference pages: [API](api.md), [configuration](configuration.md), [observability](observability.md), [fake provider](fake-provider.md), [benchmarks](benchmarks.md) and [development](development.md).

## 1. Requirements

### Functional

| Requirement | Status |
|---|---|
| `GET /healthz` (liveness) and `GET /readyz` (readiness) | Built (M0) |
| `GET /v1/models` in the OpenAI list shape: the aliases with at least one usable route | Built (M1) |
| Every error in the OpenAI error envelope ([0002](adr/0002-openai-compatible-api-and-error-envelope.md)) | Built (M0) |
| `POST /v1/chat/completions`, streamed (SSE) and not streamed; a non-streamed request is joined from an upstream stream ([0015](adr/0015-streaming-responses.md)) | Built (M1) |
| The provider contract: one streaming interface, typed upstream errors, an HTTP client with a first-byte timeout, and a contract suite every adapter runs ([0012](adr/0012-streaming-interface-callback.md)) | Built (M1) |
| Providers: Groq and OpenAI through one OpenAI-compatible adapter, with platform keys ([0005](adr/0005-providers.md), [0013](adr/0013-provider-credentials.md)) | Built (M1) |
| Provider: Ollama, on its native chat API, enabled by `OLLAMA_BASE_URL` | Built (M1) |
| Provider timings in usage: queue, load, prefill and decode, as each provider reports them (Groq: queue, prefill, decode; Ollama: load, prefill, decode). A value the provider does not report stays unset, never 0. A prompt count the provider does not report is marked unknown, never billed as 0. | Built (M1). Planned (M2): TTFT and TPOT metrics from them. |
| A fake OpenAI-compatible upstream (`fakeprovider`) with deterministic output and fault modes chosen per request in the model name, for tests and load tests | Built (M1) |
| Provider: Anthropic, and `response_format` across providers with output schema validation | Planned (M5) |
| Tenants, hashed API keys, encrypted per-tenant provider credentials | Planned (M3) |
| Routing: model aliases to an ordered provider list, from a routes table served from an in-memory snapshot ([0014](adr/0014-routes-in-postgres.md)) | Built (M1) |
| Routing resilience: failover before the first content delta, a breaker per endpoint, jittered retries that honour `Retry-After` | Planned (M4) |
| Rate limits, budget reservation and settlement, the exact cache | Planned (M7) |
| Usage metering: one usage event per request, a ledger with the price charged | Planned (M6) |
| Opt-in, redacted request logs, and eval sets built from them | Planned (M6, M9) |
| Input and output guards: PII redaction, prompt-injection flags (flag only, never block) | Planned (M10) |
| Semantic cache with a per-tenant filter | Planned (M11) |
| `gatewayctl`: `migrate up`, `down`, `status` | Built (M0) |
| `gatewayctl`: `routes add`, `list`, `enable`, `disable` | Built (M1) |
| `gatewayctl`: tenants, keys, credentials, eval | Planned (M3, M9) |

### Non-functional

| Requirement | How it is checked | Status |
|---|---|---|
| A dependency outage removes the pod from traffic and never restarts it | Measured: `/readyz` 503 within 320 ms of Postgres stopping, `/healthz` 200, no restart | Built (M0) |
| An in-flight request survives SIGTERM, inside the Kubernetes grace period | `TestInFlightRequestFinishesDuringShutdown`, `TestReadyzIs503DuringTheDrainDelay` | Built (M0) |
| No secret in logs or error bodies | Redaction tests through the real slog handlers and `encoding/json`; fixed 5xx message | Built (M0) for config and errors |
| No known vulnerability reachable from the code | `govulncheck` in CI | Built (M0) |
| The HTTP status is held for up to `STATUS_HOLD`, then heartbeats keep the stream open; a watchdog cuts a stream with no first event or a stalled one | End-to-end tests through the fake provider's fault modes, the watchdog's own tests, and an OpenAI Python SDK check | Built (M1) |
| Gateway overhead over the upstream is measured | k6 p50 and p95 against a fake upstream (`make loadtest`). Measured 2026-10-12: 1 to 3 ms p50 added at 5 ms per token and 1 or 10 VUs, about 0.2 to 0.3 ms of the gateway's own cost per request at 0 ms and 1 VU, 0 errors in every case. Results in [benchmarks.md](benchmarks.md). | Built (M1), measured |
| No usage is lost and none is counted twice when Kafka is down | Stop Kafka, send 100 requests, restart: 100 ledger rows, 0 duplicates after injected duplicates | Planned (M6) |
| Tenant A never reaches tenant B's credential, cache entry or request log | Isolation suite that attempts each access and fails if it succeeds | Planned (M3, M11) |
| Telemetry setup: OpenTelemetry tracer and meter providers exported over OTLP/HTTP, no-op when no endpoint is set, flushed after the HTTP server stops and inside the shutdown budget; log lines in a span carry its trace and span IDs ([0016](adr/0016-observability.md)) | Tests against a fake OTLP/HTTP server (no export without an endpoint, every span and metric exported on shutdown), an in-memory exporter (resource, sampler), and the JSON log output | Built (M2) |
| HTTP server spans and RED metrics (`http.server.request.duration`) with the route template, never the raw path, and a fixed `server.address`; request log lines carry the server span's trace ID | Tests with an in-memory span recorder and metric reader: span names and `http.route`, 5xx marked as an error, bounded metric attributes under a hostile `Host` header, write deadlines and flush through the instrumentation | Built (M2) |
| W3C trace context in from clients and out to providers: the trace ID continues, an incoming sampled flag `00` does not switch off recording, and provider requests carry `traceparent`; client spans and `http.client.*` metrics on provider calls; health and readiness probes get metrics but no spans ([0016](adr/0016-observability.md)) | End-to-end test through the fake provider (client flag `00` and an upstream 502 give an exported, sampled server span in the client's trace, and the provider receives the same trace ID with flag `01`); connection reuse through the instrumented client; probe spans and metrics | Built (M2) |
| GenAI time to first token, time per output chunk and token metrics | Recorded-metrics tests | Planned (M2) |
| The same manifests run on kind, EKS and GKE Autopilot | Kustomize base with an overlay per target ([0006](adr/0006-deploy-targets.md)) | Planned (M8, M12, M13) |

## 2. Back-of-envelope estimates

These are estimates from assumed planning figures, not requirements. They size the components and show where the limits are. Milestones 1, 2 and 8 replace them with measurements.

| Assumption | Value |
|---|---|
| A1. Requests per day, all tenants | 50,000 |
| A2. Peak rate over the daily average | 10x |
| A3. Mean duration of a streamed response | 10 s |
| A4. Serialized usage event | about 500 bytes |
| A5. Ledger row with its indexes, on disk | about 400 bytes |
| A6. Postgres time per request on the request path (auth lookup, outbox insert) | about 10 ms |
| A7. Opt-in request logs: share of requests, size after redaction, retention | 10%, 4 KB, 7 days |

| Estimate | Working | Result |
|---|---|---|
| Average rate | 50,000 / 86,400 s | about 0.6 requests/s |
| Peak rate | 0.6 x 10 | about 6 requests/s |
| Open streams at peak (Little's law: concurrency = rate x duration) | 6 x 10 s | about 60 |
| Busy Postgres connections at peak | 6 x 0.010 s | about 0.06 |
| `usage.v1` volume | 50,000 x 500 B | about 25 MB/day |
| Ledger growth | 50,000 x 365 x 400 B | about 18 million rows, 7.3 GB per year |
| `requestlog.v1` volume and retained size | 5,000 x 4 KB, kept 7 days | about 20 MB/day, 140 MB retained |
| Redis operations at peak (limit check, hold, settle) | 6 x 3 | about 18 ops/s |

What the estimates show:

- At this volume no component is near a throughput limit. The design is shaped by failure behaviour, tenant isolation and correct metering, not by throughput. The broker choice ([0004](adr/0004-kafka-for-the-usage-pipeline.md)) does not rest on volume.
- The pool size per replica (`DB_MAX_CONNS`, default 10) is far above the 0.06 busy connections. The real limit is scale-out: `replicas x DB_MAX_CONNS` must stay below Postgres' `max_connections` (100 by default). See section 8.
- An open stream holds a goroutine and its buffers, not a database connection, so open streams cost memory, not pool slots. Milestone 2 measures memory per stream.

## 3. Architecture

Two long-running processes, one admin binary, and a test server:

- `cmd/gateway`: the request path. **Built (M0)**: health, readiness, the models stub, graceful shutdown, the Postgres pool. **Built (M1)**: the provider registry, with one adapter per provider that has a platform key, and Ollama when `OLLAMA_BASE_URL` is set. The rest is planned.
- `cmd/worker`: everything off the request path. **Planned (M6)**.
- `cmd/gatewayctl`: one-shot admin commands. **Built (M0)**: `migrate`. **Built (M1)**: `routes`.
- `cmd/fakeprovider`: a fake OpenAI-compatible upstream with per-request fault modes (slow headers, slow first token, slow tokens, stall, drop, malformed chunk, error status), for tests and load tests. **Built (M1)**. It has its own image target, so the production image does not contain it, and Compose starts it only under the `loadtest` profile.

```mermaid
flowchart LR
    client["Client<br/>OpenAI SDK"] -->|"Bearer tenant key"| gw["cmd/gateway"]
    gw --> redis[("Redis<br/>limits, holds, exact cache")]
    gw --> pg[("Postgres<br/>tenants, keys, credentials,<br/>ledger, outbox, pgvector")]
    gw --> prov["Providers<br/>Ollama, Groq, OpenAI, Anthropic"]
    gw -.->|"traces, metrics"| otel["OTel collector<br/>Prometheus, Tempo, Grafana"]
    pg -->|"outbox rows"| wk["cmd/worker<br/>relay, consumers, reconciler"]
    wk <-->|"usage.v1, requestlog.v1"| kafka[("Kafka")]
    wk -->|"ledger rows"| pg
    wk -->|"rebuilt spend"| redis
    ctl["cmd/gatewayctl"] -->|"migrations, admin"| pg
```

### Request path

The nine steps run in `cmd/gateway`, in this order. **Planned (M1 to M11)**; the milestone for each step is in brackets.

1. Authenticate the API key, which resolves the tenant (M3).
2. Admit: check the rate limit, then reserve budget in Redis (M7).
3. Run the input guards (M10).
4. Look up the exact cache (M7), then the semantic cache (M11).
5. Route: model alias, then an ordered provider list with a breaker and retries. Failover happens only before the first content delta (M4).
6. Stream from the provider. Hold the HTTP status for up to `STATUS_HOLD`, then commit 200 and send SSE heartbeats until the first token. A watchdog cuts a stream with no first event or a stalled one. **Built (M1)**, with the first route only; failover in step 5 is M4.
7. Run the output guards, with a holdback window for streams (M10).
8. Settle the budget (M7).
9. Write the usage record and a transactional outbox row in one Postgres transaction (M6).

```mermaid
sequenceDiagram
    participant C as Client
    participant G as Gateway
    participant R as Redis
    participant P as Postgres
    participant U as Provider
    C->>G: POST /v1/chat/completions
    G->>P: 1. authenticate key, resolve tenant (cached)
    G->>R: 2. rate limit, reserve budget hold
    Note over G: 3. input guards
    G->>R: 4. exact cache lookup
    G->>P: 4. semantic cache lookup (pgvector)
    Note over G: 5. route by alias, breaker, retries
    G->>U: 6. stream request
    U-->>G: first token
    G-->>C: 200 and SSE deltas (status held until the first token)
    Note over G: 7. output guards with a holdback window
    G->>R: 8. settle budget
    G->>P: 9. usage record and outbox row, one transaction
```

Holding the status until the first token lets the gateway fail over, or return a real error status, for any failure before content starts. After the first token the status is sent, so a later failure arrives as an error event in the stream.

### Off the request path

**Planned (M6, M7, M9).** The outbox decouples the request from Kafka: a request commits its outbox row with its own records, and the relay publishes later.

```text
Postgres outbox ─ relay (FOR UPDATE SKIP LOCKED) ─> Kafka usage.v1 (key tenant_id, no prompt content)
                                                     ├─ group billing   ─> Postgres ledger, idempotent on request_id,
                                                     │                     offsets committed after the transaction,
                                                     │                     dead-letter topic for records that keep failing
                                                     └─ group analytics ─> usage aggregates for dashboards
                ─ relay ─> Kafka requestlog.v1 (opt-in, redacted before produce, short retention) ─> eval sets (M9)

reconciler: Postgres ledger ─> Redis spend
```

## 4. Data model

**Built (M0):** migration 00001 enables the `vector` extension. **Built (M1):** migration 00002 adds `routes`. **Planned:** the other tables below.

| Table | Holds | Milestone |
|---|---|---|
| `tenants` | One row per tenant | M3 |
| `api_keys` | Key prefix, key hash, `revoked_at`. The full key is never stored. | M3 |
| `provider_credentials` | Each tenant's provider keys, AES-GCM envelope encrypted (section 6) | M3 |
| `routes` | Model alias, provider, upstream model, priority, enabled, input and output prices per million tokens (`numeric`, never float), default and maximum `max_tokens` | Built (M1) |
| `budgets` | Spend limits per tenant and period | M7 |
| `usage_events` | One row per request, `request_id` primary key, with a snapshot of the price charged | M6 |
| `request_logs` | Opt-in, redacted prompts and responses | M6 |
| `outbox` | Events waiting for the relay | M6 |

### Tenant isolation: two layers

```text
request ─> API key ─> tenant_id
  layer 1, application: every query takes tenant_id from the authenticated key, never from the request body
  layer 2, Postgres row-level security: a policy on each tenant table hides rows of other tenants,
           so a query that forgets layer 1 still returns nothing it should not
```

Layer 2 works only for a role that does not bypass it. Superusers and roles with `BYPASSRLS` always bypass row-level security, and table owners bypass it unless the table uses `FORCE ROW LEVEL SECURITY`. Today the gateway connects as a superuser, so milestone 3 adds a separate app role that does not own the tables ([0010](adr/0010-migrations-with-goose.md)).

## 5. Failure modes

What happens when each dependency fails, and what the client sees.

| Dependency | Failure | Gateway behaviour | Client sees | Status |
|---|---|---|---|---|
| Postgres | Down or unreachable | `/readyz` returns 503, so Kubernetes stops routing to the pod. `/healthz` stays 200, so no restart. The pool reconnects when Postgres returns. | New requests go to other pods. If all pods are out, the load balancer has no backend. | Built (M0), measured |
| Postgres | Down, for a request already on the pod | Auth and the outbox write need Postgres. How long a cached key stays valid is decided in M3. | An error in the OpenAI envelope. | Planned (M3, M6) |
| Postgres | Pool exhausted under load | The readiness ping waits for a connection and times out after 2 s, so the pod reports not ready although Postgres is up. | Fewer pods in rotation. See section 8. | Known trade-off, M8 |
| Redis | Down | Each check fails open or closed, as decided in M7. The planned behaviour: rate limits fail open, budgets fail closed. The reconciler rebuilds spend when Redis returns. | Requests within budget proceed. A request that needs a budget hold gets an error in the OpenAI envelope. | Planned (M7) |
| Kafka | Down | Nothing on the request path. Outbox rows accumulate in Postgres. The relay publishes them when Kafka returns, and billing is idempotent on `request_id`. | Nothing. | Planned (M6) |
| Provider | Error or 429 before the first token | Retry with jitter, honouring `Retry-After`, then fail over to the next provider in the route. The breaker opens for an endpoint that keeps failing. A 401 or 403 cools down that credential, not the provider. | A normal response from another provider, or an error status in the OpenAI envelope if every provider fails. | Planned (M4) |
| Provider | Drops mid-stream | No failover after content has started. Usage is marked truncated. | An error event in the SSE stream. | Planned (M4) |
| Provider | Stalls mid-stream | The watchdog's stall deadline cuts the stream. | An error event in the SSE stream, with no `[DONE]`; a real 504 if the status was not sent yet. | Built (M1) |
| Provider | Many concurrent requests to an HTTP/1.1 upstream (Ollama, a self-hosted vLLM) | Each request needs its own connection. The upstream client keeps up to 256 idle connections per host and 1024 in total, so connections are reused. With Go's default of 2, most requests dialled a new connection: at 100 concurrent streams the gateway's ephemeral ports filled with `TIME_WAIT` sockets within about a minute, and some dials failed with `cannot assign requested address` (measured against the fake provider, 2026-10-12). Groq and OpenAI negotiate HTTP/2 and share one connection. | Normal responses. Without the pool, an occasional `502` before the status is sent. | Built (M1), measured |
| Client | Disconnects | The request context ends, which cancels the upstream call. It is logged as `client_gone` and never counts against the provider. | Nothing. | Built (M1) |

## 6. Security

| Area | Design | Status |
|---|---|---|
| API keys | Random keys, stored as a prefix and a hash, with `revoked_at`. The hash function (SHA-256 or argon2 for random keys) and the auth cache TTL against revocation speed are decided in M3. | Planned (M3) |
| Provider credentials | Platform keys come from the environment as `config.Secret` values; a tenant key, when present, wins ([0013](adr/0013-provider-credentials.md)). Tenant keys use AES-GCM envelope encryption. The associated data is `tenant_id\|provider\|key_version`, so a ciphertext copied to another tenant's row fails to decrypt. The key-encryption key moves to a cloud KMS. | Built (M1) for platform keys. Planned (M3, M12, M13) for tenant keys. |
| Secrets in config | `config.Secret` prints `[redacted]` through `fmt`, `log/slog` and `encoding/json`. `DATABASE_URL` is never logged, and a pgx parse error, which can quote the URL, is replaced with a fixed message. | Built (M0) |
| Error bodies | A 5xx returns a fixed message; the full error goes to the log. The client message never comes from `err.Error()`. | Built (M0) |
| Usage and request logs | `usage.v1` carries no prompt content. `requestlog.v1` is opt-in and redacted before it is produced, with the same PII redaction as the guards. | Planned (M6, M10) |
| Tenant isolation | Application checks plus row-level security (section 4). | Planned (M3) |
| Supply chain | Distroless static non-root image. Actions pinned by full commit SHA, with Dependabot weekly. `govulncheck` and gosec in CI. The Go toolchain pinned to a patch release with the known fixes. The CI token is read-only. | Built (M0), [0011](adr/0011-image-and-ci.md) |

## 7. Operations

**Built (M0)** unless marked.

**Probes.** `/healthz` checks only the process, so a dependency outage never restarts a pod. `/readyz` runs each readiness check with a 2 s timeout and returns 503 while the pod drains.

**Drain and shutdown** ([0008](adr/0008-graceful-shutdown.md)):

```text
SIGTERM ─> /readyz 503, still serving ─> DRAIN_DELAY ─> Shutdown (in-flight finish, up to SHUTDOWN_TIMEOUT)
        ─> exit 0, or Close and exit 1 on timeout
a second signal during the drain quits at once
```

**Shutdown budget.** `DRAIN_DELAY` (5s planned in Kubernetes, 0s locally) plus `SHUTDOWN_TIMEOUT` (20s) stays below the 30 s grace period. Planned (M8): probe timing that fits inside the drain delay, and the choice between the in-app delay and a `preStop` sleep.

**Migrations** ([0010](adr/0010-migrations-with-goose.md)). A one-shot `gatewayctl migrate up` runs before the gateway starts, under a session advisory lock.

```text
postgres (healthy) ─> migrate (exit 0) ─> gateway
                      migrate fails    ─> gateway does not start
```

**CI** ([0011](adr/0011-image-and-ci.md)). Four parallel jobs on every pull request and push to `main`: lint, test (race, with Postgres in testcontainers), vuln, image.

**Planned:** observability with the OTel collector, Prometheus, Tempo and Grafana, and dashboards as code (M2). Kubernetes probes, PodDisruptionBudget, HPA and k6 load tests on kind (M8). Cost and budget alarms in the cloud (M12, M13).

## 8. Roadmap and open gaps

| Milestone | Scope | Status |
|---|---|---|
| 0 | Walking skeleton: config, logging, health and readiness, graceful shutdown, Postgres pool and migrations, image, Compose, CI, this design | Built |
| 1 | Streaming core: provider interface, Ollama and OpenAI-compatible adapters, fake provider, status held until the first token, stall timeout | Built |
| 2 | Observability | Planned |
| 3 | Tenancy: tenants, hashed keys, key vault, credentials, database roles | Planned |
| 4 | Router resilience: aliases, failover, breaker, retries | Planned |
| 5 | Anthropic adapter, `response_format`, output schema validation | Planned |
| 6 | Usage pipeline: outbox, relay, Kafka, worker consumer groups | Planned |
| 7 | Redis: limits, budget holds, reconciler, exact cache | Planned |
| 8 | Kubernetes on kind | Planned |
| 9 | Evals | Planned |
| 10 | Guardrails | Planned |
| 11 | Semantic cache | Planned |
| 12 | AWS (EKS) via Terraform | Planned |
| 13 | GCP (GKE Autopilot) via Terraform | Planned |

Open gaps:

1. **One database role.** `migrate` and the gateway connect as the same superuser. The gateway can run DDL, and row-level security would not apply to it. Milestone 3 adds a migrator role and an app role ([0010](adr/0010-migrations-with-goose.md)).
2. **Readiness ping under pool exhaustion.** The ping shares the request pool, so a busy pool can fail readiness on every pod at once. Milestone 8 measures it under k6 load and chooses the check ([0009](adr/0009-pgx-pool-with-lazy-connect.md)).
3. **Probe timing.** With the Kubernetes defaults (`periodSeconds` 10, `failureThreshold` 3) a pod can stay ready for about 30 s after SIGTERM, longer than the drain delay. Milestone 8 sets probe timing, and milestones 12 and 13 set load balancer health checks, to fit inside it ([0008](adr/0008-graceful-shutdown.md)).
4. **Connections at scale-out.** Ten replicas with the default pool of 10 reach Postgres' default `max_connections` of 100. Milestone 8 sizes pools per replica or adds a connection pooler.
5. **Classifier baseline.** Milestone 5 moves an existing batch classification client onto the gateway. Its failure rate before the move has no controlled measurement. Milestone 5 measures it from the client's call log, before and after the move.
