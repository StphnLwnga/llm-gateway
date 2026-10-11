# Architecture decision records

Each file records one decision in a trimmed MADR form (see [0001](0001-record-decisions-as-madr-adrs.md)). A decision that changes gets a new record that supersedes the old one.

| ADR | Decision | Status | Implemented in |
|---|---|---|---|
| [0001](0001-record-decisions-as-madr-adrs.md) | Record decisions as MADR ADRs, one per decision | Accepted | S0 |
| [0002](0002-openai-compatible-api-and-error-envelope.md) | OpenAI-compatible API and error envelope | Accepted | S0 (envelope), S1 (planned) |
| [0003](0003-postgres-system-of-record-redis-admission-only.md) | Postgres as the system of record, Redis only for admission control | Accepted | S0 (pool), S3, S6, S7, S11 (planned) |
| [0004](0004-kafka-for-the-usage-pipeline.md) | Kafka (KRaft, franz-go) for the usage pipeline | Accepted | S6 (planned) |
| [0005](0005-providers.md) | Providers: Ollama, Groq, OpenAI, Anthropic | Accepted | S1, S5 (planned) |
| [0006](0006-deploy-targets.md) | Deploy targets: Compose, kind, EKS, GKE Autopilot, via Terraform | Accepted | S0 (Compose), S8, S12, S13 (planned) |
