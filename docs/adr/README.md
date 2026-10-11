# Architecture decision records

Each file records one decision in a trimmed MADR form (see [0001](0001-record-decisions-as-madr-adrs.md)). A decision that changes gets a new record that supersedes the old one.

| ADR | Decision | Status | Implemented in |
|---|---|---|---|
| [0001](0001-record-decisions-as-madr-adrs.md) | Record decisions as MADR ADRs, one per decision | Accepted | M0 |
| [0002](0002-openai-compatible-api-and-error-envelope.md) | OpenAI-compatible API and error envelope | Accepted | M0 (envelope), M1 (planned) |
| [0003](0003-postgres-system-of-record-redis-admission-only.md) | Postgres as the system of record, Redis only for admission control | Accepted | M0 (pool), M3, M6, M7, M11 (planned) |
| [0004](0004-kafka-for-the-usage-pipeline.md) | Kafka (KRaft, franz-go) for the usage pipeline | Accepted | M6 (planned) |
| [0005](0005-providers.md) | Providers: Ollama, Groq, OpenAI, Anthropic | Accepted | M1, M5 (planned) |
| [0006](0006-deploy-targets.md) | Deploy targets: Compose, kind, EKS, GKE Autopilot, via Terraform | Accepted | M0 (Compose), M8, M12, M13 (planned) |
| [0007](0007-module-layout.md) | Module layout: cmd/, internal/ by component, consumer-side interfaces | Accepted | M0 |
| [0008](0008-graceful-shutdown.md) | Graceful shutdown: readiness drain, Shutdown, then Close | Accepted | M0, M8 (probe timing, planned) |
| [0009](0009-pgx-pool-with-lazy-connect.md) | pgx pool with lazy connect | Accepted | M0 |
| [0010](0010-migrations-with-goose.md) | Migrations: goose, embedded, a one-shot step, a session advisory lock | Accepted | M0, M3 (roles, planned) |
| [0011](0011-image-and-ci.md) | Distroless static image, CI lint set, SHA-pinned actions, one test job | Accepted | M0 |
| [0012](0012-streaming-interface-callback.md) | Streaming interface: a callback per event, the adapter owns the body | Accepted | M1 |
| [0013](0013-provider-credentials.md) | Provider credentials: platform keys and tenant keys, the tenant key wins | Accepted | M1 (platform keys), M3 (planned) |
| [0014](0014-routes-in-postgres.md) | Routes in a Postgres table, served from an in-memory snapshot | Accepted | M1, M4 (planned) |
| [0015](0015-streaming-responses.md) | Streaming responses: status hold, heartbeats and a liveness watchdog | Accepted | M1 |
