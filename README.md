# llm-gateway

A multi-tenant, OpenAI-compatible LLM gateway in Go: each tenant calls one API with its own key, and the gateway routes the request to Ollama, Groq, OpenAI or Anthropic with rate limits, budgets, usage metering and tracing.

Status: Milestone 0 (walking skeleton) in progress.

## Design

- [docs/design.md](docs/design.md): requirements, estimates, architecture, data model, failure modes, security, operations, and the roadmap.
- [docs/adr/](docs/adr/README.md): one architecture decision record per decision, with the options considered and the reasons.

## Run locally

Needs Docker with Compose.

```bash
make up                              # build the image; start Postgres, run migrations, start the gateway
curl -i localhost:8080/readyz        # 200 {"status":"ready"} once Postgres answers
make logs                            # follow the gateway's logs
make migrate-status                  # list migrations and whether each is applied
make down                            # stop the stack; the Postgres volume is kept
```

`make test` runs every test. The Postgres integration tests start a throwaway container with testcontainers, so they need Docker. Without Docker they skip locally, and fail when `CI` is set.

Migrations run as a separate one-shot step (`gatewayctl migrate up`), never when the gateway starts. The gateway starts even when Postgres is down; `/readyz` returns 503 until Postgres answers.

## Endpoints

| Method and path | What it does |
|---|---|
| `GET /healthz` | Liveness. `200 {"status":"ok"}` while the process serves HTTP. It never checks a dependency. |
| `GET /readyz` | Readiness. `200 {"status":"ready"}` when every readiness check passes; `503` while the server drains or when a check fails. |
| `GET /v1/models` | The OpenAI model list. A stub for now: `{"object":"list","data":[]}`. |

Every error uses the OpenAI error shape. An unknown route returns `404`, and a known path with the wrong method returns `405` with an `Allow` header:

```json
{"error":{"message":"Invalid URL (GET /v1/nope)","type":"invalid_request_error","param":null,"code":null}}
```

This 404 body matches OpenAI's own. A `500` returns a fixed message and never the internal error text; the gateway logs the full error instead.

## Configuration

Settings come from environment variables. Both binaries, `gateway` and `gatewayctl`, read the same settings. Every invalid setting is reported together at startup, and the gateway exits with code 1.

| Variable | Default | Accepted values |
|---|---|---|
| `DATABASE_URL` | none, required | A Postgres connection string, for example `postgres://user:password@host:5432/db`. Never printed or logged. |
| `DB_MAX_CONNS` | `10` | A whole number from 1 to 1000: the most connections the pool opens |
| `PORT` | `8080` | A number from 1 to 65535 |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `LOG_FORMAT` | `json` | `json` or `text` |
| `DRAIN_DELAY` | `0s` | A Go duration of 0 or more, for example `5s`. After SIGTERM, how long `/readyz` returns 503 while the server still serves, so the load balancer stops sending traffic. |
| `SHUTDOWN_TIMEOUT` | `20s` | A Go duration of 0 or more. How long in-flight requests get to finish after the drain. Keep `DRAIN_DELAY` plus `SHUTDOWN_TIMEOUT` below the Kubernetes grace period (30s by default). |

## Shutdown

On SIGINT or SIGTERM the gateway marks itself as draining (`/readyz` returns 503), waits `DRAIN_DELAY`, stops accepting connections, and gives in-flight requests up to `SHUTDOWN_TIMEOUT` to finish. A signal never cancels a request that is already running; only the timeout does.

## CI

Every pull request and every push to `main` runs four jobs in GitHub Actions (`.github/workflows/ci.yml`):

| Job | What it runs | Fails when |
|---|---|---|
| lint | `gofmt -l`, `go mod tidy -diff`, `go vet`, golangci-lint v2 (the standard set plus gosec, errorlint, bodyclose, noctx, revive, misspell) | a file is not formatted, `go.mod` is not tidy, or any check reports a finding |
| test | `go test -race ./...`, including the Postgres integration tests on the runner's Docker | a test fails or the race detector finds a race |
| vuln | `govulncheck ./...` | the code reaches a known vulnerability |
| image | `docker build` of `deploy/docker/Dockerfile`, no push | the image does not build |

`readme_test.go` fails when a setting, a route or a Go package folder is missing from this README. Actions are pinned by commit SHA, and Dependabot proposes updates to actions and Go modules weekly. A new push to the same branch cancels the run in progress.

## Project layout

```text
cmd/gateway/                 the HTTP server: composition root, graceful shutdown
cmd/gatewayctl/              one-shot admin commands: migrate up, down, status
internal/config/             settings parsed into typed values; Secret redaction
internal/httpapi/            routes, readiness checks, the OpenAI error shape, server timeouts
internal/store/postgres/     the connection pool, the readiness check, embedded migrations
internal/telemetry/          the structured logger
deploy/docker/Dockerfile     two-stage build into a distroless, non-root image
deploy/compose.yaml          the local stack: Postgres (pgvector), migrate, gateway
readme_test.go               fails when this README misses a setting, route or package
docs/design.md               the system design
docs/adr/                    architecture decision records
.github/                     CI workflow, Dependabot, pull request template
.golangci.yml                lint settings
```
