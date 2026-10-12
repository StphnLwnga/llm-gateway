# Development

## Run locally

You need Docker with Compose.

```bash
make up                              # build the image; start Postgres, run migrations, start the gateway
curl -i localhost:8080/readyz        # 200 {"status":"ready"} once Postgres answers
make logs                            # follow the gateway's logs
make migrate-status                  # list migrations and whether each is applied
make down                            # stop the stack, with or without the observability services; volumes are kept
```

Provider keys go in a `.env` file at the repo root, for example `GROQ_API_KEY=...`. Compose passes the file to the gateway if it exists. Git ignores the file, and the image never contains it. Compose points `OLLAMA_BASE_URL` at Ollama on the host (`http://host.docker.internal:11434`).

Migrations run as a separate one-shot step (`gatewayctl migrate up`), never when the gateway starts. The gateway starts even when Postgres is down. `/readyz` returns 503 until Postgres answers.

The [configuration page](configuration.md) lists every setting.

## Observability stack

`make up-obs` starts the same stack plus an OpenTelemetry Collector, Prometheus, Tempo and Grafana, from `deploy/compose.observability.yaml`. It sets `OTEL_EXPORTER_OTLP_ENDPOINT` on the gateway, so the gateway exports its traces and metrics to the collector. `make up` leaves them out, and the gateway then exports nothing. The [observability page](observability.md) lists the spans and metrics.

```text
 gateway ──OTLP/HTTP──> collector ──metrics──> Prometheus
                            └──────traces───> Tempo
                                Grafana queries both
```

| Service | Address on the host | Notes |
|---|---|---|
| Grafana | http://localhost:3300 | User `admin`, password `grafana-local`, or `GRAFANA_ADMIN_PASSWORD` when it is set. Prometheus and Tempo are provisioned as data sources. |
| Prometheus | http://localhost:9090 | Receives the collector's metrics over OTLP, and scrapes the collector's own metrics |
| Tempo | http://localhost:3200 | Trace queries |
| Collector | http://localhost:4318 | OTLP/HTTP, for a gateway run on the host: `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 go run ./cmd/gateway` |

Every port is bound to 127.0.0.1. The collector, Prometheus and Tempo have no authentication, and Grafana uses a local default password. Prometheus and Tempo keep 7 days of data in named volumes, so `make down` keeps them.

The collector tail-samples traces. It keeps every trace with an error, every trace longer than 30 s, and 10% of the rest. It decides 5 s after a trace's root span arrives, and at most 300 s after its first span. A request that continues a client's trace has no root span in this collector, so its trace waits the full 300 s. Most traces of successful requests never reach Tempo, and a kept trace is searchable in Tempo up to about 30 s later. Metrics are not sampled. The configs are in `deploy/observability/`.

To check the stack without the gateway, send test data with telemetrygen:

```bash
TG=ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.162.0@sha256:5350f77dbc914034321dc7629058e141996c8dc2cdde076d281e573efee1a67e
docker run --rm --network llm-gateway_default "$TG" traces --otlp-endpoint otel-collector:4318 --otlp-http --otlp-insecure --traces 5 --status-code Error
```

A config file mounted into a container must exist before `make up-obs`. When the host file is missing, Docker creates an empty directory in its place, and the service fails later with an error such as "is a directory". `test -f` on each mounted file catches it first.

## Tests

`make test` runs every test. The Postgres integration tests start a throwaway container with testcontainers, so they need Docker. Without Docker, they skip locally and fail when `CI` is set.

## Shutdown

On SIGINT or SIGTERM, the gateway does these steps in order:

1. It marks itself as draining, so `/readyz` returns 503.
2. It waits for `DRAIN_DELAY`.
3. It stops accepting connections.
4. It gives in-flight requests up to `SHUTDOWN_TIMEOUT` to finish.

A signal never cancels a request that is already running. Only the timeout does.

## CI

Every pull request and every push to `main` runs four jobs in GitHub Actions (`.github/workflows/ci.yml`):

| Job | What it runs | Fails when |
|---|---|---|
| lint | `gofmt -l`, `go mod tidy -diff`, `go vet`, golangci-lint v2 (the standard set plus gosec, errorlint, bodyclose, noctx, revive, misspell) | a file is not formatted, `go.mod` is not tidy, or any check reports a finding |
| test | `go test -race ./...`, including the Postgres integration tests on the runner's Docker | a test fails or the race detector finds a race |
| vuln | `govulncheck ./...` | the code reaches a known vulnerability |
| image | `docker build` of `deploy/docker/Dockerfile`: the production image and the `fakeprovider` target, no push | an image does not build |

`readme_test.go` checks that the docs stay complete:

| Item | Must have a row in |
|---|---|
| A setting | [configuration.md](configuration.md) |
| An endpoint | [api.md](api.md) |
| A fake provider directive | [fake-provider.md](fake-provider.md) |
| A Go package folder | The README project layout |

Actions are pinned by commit SHA. Dependabot proposes updates to actions and Go modules weekly. A new push to the same branch cancels the run in progress.

## Makefile

| Target | What it does |
|---|---|
| `make build` | Builds both binaries into `bin/` |
| `make test` | Runs every test, the integration tests included (needs Docker) |
| `make up` | Builds the image and starts Postgres, the migration and the gateway |
| `make up-obs` | Like `make up`, plus the collector, Prometheus, Tempo and Grafana; the gateway exports to the collector |
| `make down` | Stops the stack, with or without the observability services; volumes are kept |
| `make logs` | Follows the gateway's logs |
| `make migrate-status` | Shows which migrations are applied |
| `make loadtest` | Measures the latency the gateway adds, against the fake provider ([benchmarks](benchmarks.md)) |
| `make loadtest-down` | Removes the loadtest stack and its Postgres volume |
