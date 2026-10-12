# llm-gateway

A multi-tenant, OpenAI-compatible LLM gateway in Go: each tenant calls one API with its own key, and the gateway routes the request to Ollama, Groq, OpenAI or Anthropic with rate limits, budgets, usage metering and tracing.

Status: Milestone 0 (walking skeleton) is built. Milestone 1 (streaming core) is in progress.

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

Provider keys go in a `.env` file at the repo root (for example `GROQ_API_KEY=...`). Compose passes it to the gateway if it exists; the file is gitignored and never copied into the image. Compose points `OLLAMA_BASE_URL` at Ollama on the host (`http://host.docker.internal:11434`).

Migrations run as a separate one-shot step (`gatewayctl migrate up`), never when the gateway starts. The gateway starts even when Postgres is down; `/readyz` returns 503 until Postgres answers.

## Endpoints

| Method and path | What it does |
|---|---|
| `GET /healthz` | Liveness. `200 {"status":"ok"}` while the process serves HTTP. It never checks a dependency. |
| `GET /readyz` | Readiness. `200 {"status":"ready"}` when every readiness check passes; `503` while the server drains, when Postgres does not answer, or until the routes table has loaded once. |
| `GET /v1/models` | The OpenAI model list: every alias in the routes table that has at least one enabled route with an enabled provider. `created` is the earliest route's creation time; `owned_by` is `llm-gateway`. |
| `POST /v1/chat/completions` | OpenAI chat completions, streamed (SSE) or not. `model` is a route alias. See Chat completions below. |

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
| `PROVIDER_CONNECT_TIMEOUT` | `5s` | A Go duration above 0. How long to wait for a TCP connection to a provider. |
| `PROVIDER_FIRST_BYTE_TIMEOUT` | `60s` | A Go duration above 0. How long to wait for a provider's response headers. Some providers send headers before the first token, so this is not a time-to-first-token limit; `PROVIDER_FIRST_TOKEN_TIMEOUT` is. There is no limit on the whole response. The default leaves room for a local model that has to load before it answers. |
| `PROVIDER_FIRST_TOKEN_TIMEOUT` | `60s` | A Go duration above 0. How long a stream may wait for its first event, reasoning progress included. When it passes, the request fails with a timeout. |
| `STREAM_STALL_TIMEOUT` | `30s` | A Go duration above 0. The longest gap allowed between two stream events. |
| `STATUS_HOLD` | `3s` | A Go duration of 0 or more. How long a streamed request waits for the first token before it sends `200` and starts heartbeats. An error in that time gets a real HTTP status. |
| `HEARTBEAT_INTERVAL` | `10s` | A Go duration above 0. The gap between SSE comment heartbeats (`: heartbeat`) while a committed stream waits for its first token. |
| `REQUEST_MAX_BYTES` | `4194304` | A whole number of bytes from 1024 to 67108864: the largest request body the gateway reads. |
| `GROQ_API_KEY` | none | The platform key for Groq. When it is not set, Groq is disabled and the gateway logs that at startup. Never printed or logged. |
| `GROQ_BASE_URL` | `https://api.groq.com/openai/v1` | An http or https URL: Groq's OpenAI-compatible API, or the fake provider in tests |
| `OPENAI_API_KEY` | none | The platform key for OpenAI. When it is not set, OpenAI is disabled and the gateway logs that at startup. Never printed or logged. |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | An http or https URL: OpenAI's API, or a compatible endpoint |
| `OLLAMA_BASE_URL` | none | An http or https URL, for example `http://localhost:11434`. Ollama is enabled only when this is set; it needs no key. From inside Compose, Ollama on the host is `http://host.docker.internal:11434`. |
| `ROUTES_REFRESH_INTERVAL` | `10s` | A Go duration above 0. How often the gateway reloads the routes table into memory. A change made with `gatewayctl routes` takes effect within one interval. |

## Chat completions

`POST /v1/chat/completions` takes the OpenAI request shape, so the OpenAI SDKs work unchanged with `base_url` pointed at the gateway. `model` is a route alias; an unknown alias returns `404` with code `model_not_found`. The gateway uses the alias's first route; failover across routes comes in milestone 4.

Supported fields: `messages` (string content, or arrays of text parts), `stream`, `stream_options.include_usage`, `temperature`, `top_p`, `max_completion_tokens` or `max_tokens`, `stop`, `seed`, and `n` of 1. Unknown fields are ignored. `tools`, `response_format` and `logprobs` return `400` until they are supported. Without a token limit, the route's default applies; a limit above the route's maximum returns `400`.

**Streaming and the status.** A streamed request holds its HTTP status for up to `STATUS_HOLD` (3s):

```text
first token within the hold  -> 200, then the tokens
an upstream error first      -> a real status: 429 (with Retry-After), 502, 504 or 400
the hold passes              -> 200, then ": heartbeat" comments every HEARTBEAT_INTERVAL until the first token
an error after the 200       -> data: {"error":{...}} and the stream ends, with no data: [DONE]
```

A watchdog cuts a stream that sends no event within `PROVIDER_FIRST_TOKEN_TIMEOUT` or goes silent for longer than `STREAM_STALL_TIMEOUT`. Reasoning models keep it alive while they think. A client that disconnects cancels the upstream request.

**Non-streamed requests** are served from an upstream stream too, joined into one `chat.completion` response. The status is sent only at the end, so every error gets a real status, and a long answer has no total time limit.

Every response carries `X-Request-Id` and `X-LLM-Gateway-Provider`, and the gateway logs one line per request with the provider, the status, the time to first token, the usage and whether the stream was cut short.

```bash
curl -N localhost:8080/v1/chat/completions \
  -d '{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

Upstream errors map to a fixed public message; the upstream's own text goes only to the log:

| Upstream | Client gets |
|---|---|
| 429 rate limited | `429`, with the upstream `Retry-After` |
| 401 or 403 | `502` (the gateway's credential, not the client's) |
| other 4xx | `400` "The upstream provider rejected the request." |
| 5xx, malformed body, connection error | `502` |
| first-byte, first-token or stall timeout | `504` |

## Routes

A route maps a model alias, the name a client sends as `model`, to one provider and its upstream model. One alias can have several routes; the lowest `priority` is tried first. Routes live in the `routes` table, and `gatewayctl routes` manages them:

```bash
gatewayctl routes add --alias chat --provider groq --upstream-model openai/gpt-oss-20b --priority 10 \
  --input-price 0.05 --output-price 0.08 --default-max-tokens 512 --max-max-tokens 4096
gatewayctl routes add --alias chat --provider ollama --upstream-model llama3.2:3b --priority 20
gatewayctl routes list
gatewayctl routes disable 1
```

Prices are US dollars per million tokens, with at most six decimal places, and are stored exactly: `numeric` in Postgres and integer micro-dollars in Go. A route can only be added for `groq`, `openai` or `ollama`, with prices of 0 or more, and `--max-max-tokens` not below `--default-max-tokens`.

The gateway keeps the enabled routes in memory and reloads them every `ROUTES_REFRESH_INTERVAL`, so a request never waits on Postgres to find its route. A change made with `gatewayctl` takes effect within one interval.

```text
gatewayctl routes add ──> Postgres routes table
                              │  every ROUTES_REFRESH_INTERVAL
                              v
gateway: in-memory snapshot (swapped atomically) ──> /v1/models, routing
         a failed reload keeps the last good snapshot
```

A route whose provider is not enabled (no key, or no `OLLAMA_BASE_URL`) is skipped, and the gateway logs it once when that set of routes changes. `/readyz` returns 503 until the routes table has loaded once. After that, a failed reload does not make the pod unready: it keeps serving the last good snapshot.

## Shutdown

On SIGINT or SIGTERM the gateway marks itself as draining (`/readyz` returns 503), waits `DRAIN_DELAY`, stops accepting connections, and gives in-flight requests up to `SHUTDOWN_TIMEOUT` to finish. A signal never cancels a request that is already running; only the timeout does.

## Fake provider

`fakeprovider` serves a fake OpenAI chat completions API for local and load tests. Tests run the same handler in-process (`internal/fakeupstream`). It has its own image, the Dockerfile's `fakeprovider` target, so the production image does not contain it. The output is deterministic: the answer is a fixed word sequence, and the usage counts match the tokens sent.

```bash
docker compose -f deploy/compose.yaml --profile loadtest up -d fakeprovider   # a plain `up` does not start it
curl -N localhost:8081/v1/chat/completions \
  -d '{"model":"fake?tokens=3&tpot=50ms","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

It serves `POST /v1/chat/completions`, streamed or not (`stream_options.include_usage` adds the final usage chunk), and `GET /v1/models`, which lists the one model, `fake`. It reads one setting, `PORT` (default `8081`), and on SIGINT or SIGTERM gives open requests 5 seconds before it closes them.

**Fault modes.** The model name carries directives after a `?`, for example `fake?ttfb=2s&tokens=50&tpot=20ms`. The gateway forwards the upstream model name unchanged, so a client can choose the behaviour per request. An unknown or invalid directive returns 400 in the OpenAI error shape.

| Directive | Value | What it does |
|---|---|---|
| `ttfb` | duration | Waits before the response headers |
| `first_token` | duration | Sends the headers at once, then waits before the first chunk |
| `tpot` | duration | Waits between tokens |
| `tokens` | count, default 5 | The number of tokens in the answer. A smaller `max_completion_tokens` (or the older `max_tokens`) in the request cuts it, with finish reason `length`. |
| `stall_after` | count | Sends that many tokens, then nothing until the client goes away |
| `drop_after` | count | Sends that many tokens, then closes the TCP connection with no clean end |
| `malformed_after` | count | Sends that many tokens, then one chunk that is not valid JSON, and ends |
| `status` | 400 to 599 | Answers with that status and an OpenAI error body, after `ttfb` |
| `retry_after` | seconds | With `status`, sets the `Retry-After` header |

A duration is from 0 to 10m, and a count from 0 to 100000. Set at most one of `stall_after`, `drop_after` and `malformed_after`; its count must not be more than `tokens`. Every delay ends when the client disconnects.

```text
fake?first_token=300ms&tokens=3&tpot=50ms

t=0       200, text/event-stream headers
t=300ms   role chunk, then "The"
t=350ms   " quick"
t=400ms   " brown", then the stop chunk and [DONE]

fake?tokens=5&drop_after=2:     role, "The", " quick", then the connection closes
fake?tokens=5&stall_after=2:    role, "The", " quick", then nothing until the client leaves
```

## Measured results

### Gateway overhead

The gateway overhead is the latency that the gateway adds to a request. The test sends the same requests from k6 straight to the fake provider, then through the gateway to the same fake provider. The difference between the two is the overhead.

```bash
make loadtest        # build, start the loadtest stack, run all 24 cases, print two tables (about 18 minutes)
make loadtest-down   # remove the loadtest stack and its Postgres volume
```

`make loadtest` runs `loadtests/run.sh`. It starts a separate Compose project, `llm-gateway-loadtest`, from `deploy/compose.yaml` plus `deploy/compose.loadtest.yaml`. That project has its own network and Postgres volume and publishes no ports, so it can run next to the normal stack. Its gateway reads no `.env`: `OPENAI_BASE_URL` points at the fake provider, with a placeholder `OPENAI_API_KEY`. The normal stack never points at the fake. For each token delay, the script adds a route alias (`loadtest-fake-5ms`, `loadtest-fake-0ms`) with `gatewayctl routes add` if it is not there, so a second run reuses it. k6 (`grafana/k6:2.3.0`, pinned by digest) runs as a container on the same Compose network, so both paths cross the same network and the only difference is the gateway hop.

The cases are 2 token delays (5 ms, 0 ms) x 2 paths (direct, gateway) x 2 modes (stream, non-stream) x 3 load levels (1, 10 and 100 virtual users). Each case is a separate k6 run. The direct and gateway runs alternate at each level, so a change in the machine affects both paths.

Method:

- **Fake settings.** Every case asks for 20 tokens with no extra delay before the first token. The 5 ms table uses `fake?tokens=20&tpot=5ms`, which is closer to a real model: the fake alone took about 110 ms per answer in a trial run (19 gaps of 5 ms, plus the timer overshoot of each sleep), so the overhead is small next to it. The 0 ms table uses `fake?tokens=20&tpot=0ms`, so the fake answers at once and the gateway's own cost is most of the time. `TPOTS` (default `"5ms 0ms"`), `TOKENS` and `VUS_LEVELS` change them.
- **Load model.** Closed loop: each virtual user sends its next request when the previous one ends, with no think time. At 100 users, the request rate depends on the latency.
- **Warm-up.** Each case runs 10 s of warm-up, which is not recorded, then 30 s of measurement at the same load. `WARMUP_S` and `DURATION_S` change them.
- **The number.** The latency is k6's `http_req_duration` for the full response: from the request sent to the last byte read. k6 reads the whole body. It does not see the SSE events one by one.
- **No time to first byte.** The fake sends stream headers at once. The gateway holds them until the first token (`STATUS_HOLD`). So TTFB measures different events on the two paths, and the table leaves it out.
- **Non-stream through the gateway.** The gateway reads a stream from the fake and joins it. The direct path asks the fake for a non-streamed answer. Both take the same token delays.
- **Errors.** A response counts only if it is `200` and complete: a stream must end with `data: [DONE]` and carry the 20 expected tokens; a non-streamed answer must carry the same text and finish reason `stop`. The error column is the share of responses that fail this check. A case with errors is not a valid result.
- **Upstream connections.** The gateway's upstream HTTP client keeps up to 256 idle connections per provider host (1024 in total). Go's default is 2. Groq and OpenAI negotiate HTTP/2, so their requests share one connection either way. An HTTP/1.1 upstream without TLS (Ollama, a self-hosted vLLM, the fake) carries one request per connection, so with only 2 kept idle, most concurrent requests open a new connection. At 100 virtual users that filled the gateway's ephemeral port range with sockets in `TIME_WAIT`, and some dials failed with `cannot assign requested address`, which clients saw as `502`. The `TW peak` column is the most gateway sockets to the fake in `TIME_WAIT` during a case, sampled every second by a sidecar container that shares the gateway's network namespace. It stays near 0 while the pool works.
- **Resources.** The script samples `docker stats` for the whole run and prints the peak memory and CPU of the gateway and the fake provider, and whether either container was killed or restarted. The gateway runs with `mem_limit: 128m` and the fake with `64m`. Neither has a CPU limit. k6, the gateway and the fake share the CPUs of the Docker VM.
- **What is not on the path.** A request does not touch Postgres: routes come from the in-memory snapshot, so `DB_MAX_CONNS` does not limit this test. The gateway writes one log line per request at `info`, as it does in normal use.

Results, from the run on the machine below. Pending: the tables are filled in from the first official run.

Machine: Apple M4 (10 cores), Docker Desktop, with 10 CPUs and 7.7 GiB for containers.

5 ms between tokens (`fake?tokens=20&tpot=5ms`):

| Mode | VUs | Direct p50 (ms) | Direct p95 (ms) | Gateway p50 (ms) | Gateway p95 (ms) | Added p50 (ms) | Added p95 (ms) | Errors | TW peak |
|---|---|---|---|---|---|---|---|---|---|
| stream | 1 | | | | | | | | |
| stream | 10 | | | | | | | | |
| stream | 100 | | | | | | | | |
| non-stream | 1 | | | | | | | | |
| non-stream | 10 | | | | | | | | |
| non-stream | 100 | | | | | | | | |

0 ms between tokens (`fake?tokens=20&tpot=0ms`):

| Mode | VUs | Direct p50 (ms) | Direct p95 (ms) | Gateway p50 (ms) | Gateway p95 (ms) | Added p50 (ms) | Added p95 (ms) | Errors | TW peak |
|---|---|---|---|---|---|---|---|---|---|
| stream | 1 | | | | | | | | |
| stream | 10 | | | | | | | | |
| stream | 100 | | | | | | | | |
| non-stream | 1 | | | | | | | | |
| non-stream | 10 | | | | | | | | |
| non-stream | 100 | | | | | | | | |

## CI

Every pull request and every push to `main` runs four jobs in GitHub Actions (`.github/workflows/ci.yml`):

| Job | What it runs | Fails when |
|---|---|---|
| lint | `gofmt -l`, `go mod tidy -diff`, `go vet`, golangci-lint v2 (the standard set plus gosec, errorlint, bodyclose, noctx, revive, misspell) | a file is not formatted, `go.mod` is not tidy, or any check reports a finding |
| test | `go test -race ./...`, including the Postgres integration tests on the runner's Docker | a test fails or the race detector finds a race |
| vuln | `govulncheck ./...` | the code reaches a known vulnerability |
| image | `docker build` of `deploy/docker/Dockerfile`: the production image and the `fakeprovider` target, no push | an image does not build |

`readme_test.go` fails when a setting, a route, a Go package folder or a fake provider directive is missing from this README. Actions are pinned by commit SHA, and Dependabot proposes updates to actions and Go modules weekly. A new push to the same branch cancels the run in progress.

## Project layout

```text
.
├── cmd/
│   ├── fakeprovider/        the fake upstream as a server, for local and load tests
│   ├── gateway/             the HTTP server: composition root, graceful shutdown
│   └── gatewayctl/          one-shot admin commands: migrate up, down, status; routes add, list, enable, disable
├── internal/
│   ├── config/              settings parsed into typed values; Secret redaction
│   ├── fakeupstream/        a fake OpenAI chat completions server with per-request fault modes
│   ├── httpapi/             routes, readiness checks, the OpenAI error shape, server timeouts
│   ├── money/               exact US dollar amounts in micro-USD, for prices and costs
│   ├── provider/            the provider contract: request and event types, the Provider interface, typed upstream errors, the upstream HTTP client
│   │   ├── ollama/          the adapter for Ollama's native chat API, with load, prefill and decode timings
│   │   ├── openaicompat/    the adapter for OpenAI-compatible APIs (Groq, OpenAI), with recorded and constructed wire fixtures
│   │   └── providertest/    the contract suite every adapter runs against a fake upstream
│   ├── router/              model alias to providers, from an in-memory snapshot of the routes table
│   ├── sse/                 Server-Sent Events writer with a write deadline per event, and heartbeats
│   ├── store/
│   │   └── postgres/        the connection pool, the readiness check, embedded migrations, the routes table
│   └── telemetry/           the structured logger
├── deploy/
│   ├── docker/
│   │   └── Dockerfile       distroless, non-root images: the gateway (default target) and the fake provider
│   ├── compose.yaml         the local stack: Postgres (pgvector), migrate, gateway
│   └── compose.loadtest.yaml the overhead test stack: the gateway pointed at the fake provider, k6, and a socket counter
├── docs/
│   ├── design.md            the system design
│   └── adr/                 architecture decision records
├── loadtests/
│   ├── k6/
│   │   └── overhead.js      one overhead case: one path, one mode, one VU level
│   └── run.sh               runs every overhead case and prints the tables (make loadtest)
├── .github/                 CI workflow, Dependabot, pull request template
├── .golangci.yml            lint settings
└── readme_test.go           fails when this README misses a setting, route, package or fake directive
```
