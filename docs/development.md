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

### Database roles

`deploy/postgres/initdb/10-roles.sh` creates three login roles, and each Compose service connects as one of them. The `postgres` superuser runs only the init script.

| Role | Service | Rights |
|---|---|---|
| `gateway_migrator` | `migrate` | Owns the database and the tables, and runs DDL. Has `BYPASSRLS`. `gatewayctl migrate` refuses any role that does not own the database, with "migrate needs the migrator role; use the migrate service". |
| `gateway_app` | `gateway` | Only what each migration grants: today `SELECT` on `routes` and `EXECUTE` on `auth_lookup`. No rights on `tenants` or `api_keys`. It cannot create a table or read `goose_db_version`. |
| `gateway_admin` | `gatewayctl` | Only what each migration grants: today `SELECT`, `INSERT`, `UPDATE` and `DELETE` on `routes`, and `SELECT`, `INSERT` and `UPDATE` on `tenants` and `api_keys`. Has `BYPASSRLS`. |

The gateway refuses a role that is a superuser, has `BYPASSRLS`, or owns a table in schema `public`, directly or through a role it can become. `/readyz` then returns 503, and its log line names the rule that failed. The passwords have local defaults in `compose.yaml`; `deploy/.env.example` lists the keys that override them.

The `gatewayctl` service runs admin commands. It is in the `tools` profile, so `make up` does not start it:

```bash
docker compose -f deploy/compose.yaml run --rm gatewayctl routes list
```

Postgres runs the init script only when its data volume is empty. After a change to the script, remove the volume so the script runs again. This deletes the local data:

```bash
make down
docker volume rm llm-gateway_pgdata
make up
```

### Tenants and API keys

Every `/v1/` request needs an API key ([API](api.md#authentication)). `gatewayctl` creates tenants and keys through the `gatewayctl` service:

```bash
CTL='docker compose -f deploy/compose.yaml run --rm gatewayctl'
$CTL tenants create --name acme             # tenant <tenant ID> created: acme
$CTL tenants list                           # ID, name, created, disabled
$CTL keys create --tenant <tenant ID>       # prints the key once; store it then; refuses a disabled tenant
$CTL keys list --tenant <tenant ID>         # ID, prefix, created, revoked; never the key or its hash
$CTL keys revoke <key ID>                   # the key stops working on its next request
$CTL tenants disable <tenant ID>            # every key of the tenant stops working
```

IDs are UUIDv7. A revoke or a disable keeps the row, and running it again keeps the first time. Nothing deletes a tenant or a key.

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
| Grafana | http://localhost:3300 | User `admin`, password `grafana-local`, or `GRAFANA_ADMIN_PASSWORD` when it is set. Prometheus and Tempo are provisioned as data sources. `GF_SERVER_ROOT_URL` must match the host port, or share links point to the wrong port. |
| Prometheus | http://localhost:9090 | Receives the collector's metrics over OTLP, and scrapes the collector's own metrics |
| Tempo | http://localhost:3200 | Trace queries |
| Collector | http://localhost:4318 | OTLP/HTTP, for a gateway run on the host: `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 go run ./cmd/gateway` |

Every port is bound to 127.0.0.1. The collector, Prometheus and Tempo have no authentication, and Grafana uses a local default password. Prometheus and Tempo keep 7 days of data in named volumes, so `make down` keeps them.

The collector tail-samples traces. It keeps every trace with an error, every trace longer than 30 s, every trace with a first chunk slower than 1 s, and 10% of the rest. `TRACE_SAMPLE_PERCENT=100 make up-obs` keeps every trace ([observability](observability.md#genai-span)). It decides 5 s after a trace's root span arrives, and at most 300 s after its first span. A request that continues a client's trace has no root span in this collector, so its trace waits the full 300 s. Most traces of successful requests never reach Tempo, and a kept trace is searchable in Tempo up to about 30 s later. Metrics are not sampled. The configs are in `deploy/observability/`.

To check the stack without the gateway, send test data with telemetrygen:

```bash
TG=ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.162.0@sha256:5350f77dbc914034321dc7629058e141996c8dc2cdde076d281e573efee1a67e
docker run --rm --network llm-gateway_default "$TG" traces --otlp-endpoint otel-collector:4318 --otlp-http --otlp-insecure --traces 5 --status-code Error
```

A config file mounted into a container must exist before `make up-obs`. When the host file is missing, Docker creates an empty directory in its place, and the service fails later with an error such as "is a directory". `test -f` on each mounted file catches it first.

### Dashboards

Grafana loads two dashboards from `deploy/observability/grafana/dashboards/` into the folder `llm-gateway`:

| Dashboard | Address | Shows |
|---|---|---|
| Gateway | http://localhost:3300/d/llm-gateway | Requests by route, provider calls, and stream outcomes that the status code cannot show |
| Telemetry pipeline | http://localhost:3300/d/llm-gateway-pipeline | The collector: spans in and refused, sampling decisions, export queues, send failures and memory |

The files are the source. The UI cannot save over them. Grafana has no volume, so anything saved only in the UI is lost when the container is recreated. Alert rules come with milestone 4.

The panels use `$__rate_interval`, which is at least 1 minute here. `make up-obs` makes the gateway export metrics every 15 s (`OTEL_METRIC_EXPORT_INTERVAL`), the same as the Prometheus scrape interval, and the data source sets `timeInterval: 15s` to match. A shorter export interval than the SDK default of 60 s keeps short load steps visible.

To change a dashboard:

1. Load the file into Grafana as a draft with another uid:
   ```bash
   python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); d["uid"]="draft-"+d["uid"]; d["title"]+=" draft"; print(json.dumps({"dashboard":d,"overwrite":True}))' \
     deploy/observability/grafana/dashboards/gateway.json \
     | curl -s -u admin:grafana-local -H 'Content-Type: application/json' localhost:3300/api/dashboards/db -d @-
   ```
2. Edit the draft in the UI, and save it.
3. Export it: **Export**, **Export as code**, **Advanced options**, **Model: Classic**. Keep **Export for sharing externally** off, so the data source uids stay `prometheus` and `tempo`.
4. In a file outside the dashboards folder, remove the top-level `id` and `version`, and set `uid` and `title` back to the old values. Grafana reads the folder every 10 s, so a half-edited file there goes live.
5. Move the file over the old one. Then delete the draft: `curl -s -u admin:grafana-local -X DELETE localhost:3300/api/dashboards/uid/draft-<uid>`.

A provisioned dashboard cannot be exported after an edit. Grafana asks you to save it first, and the JSON it offers instead uses the V2 schema.

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
