# llm-gateway

A multi-tenant, OpenAI-compatible LLM gateway in Go: each tenant calls one API with its own key, and the gateway routes the request to Ollama, Groq, OpenAI or Anthropic with rate limits, budgets, usage metering and tracing.

Status: S0 (walking skeleton) in progress.

Design: [docs/design.md](docs/design.md).

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

Settings come from environment variables. Every invalid setting is reported together at startup, and the gateway exits with code 1.

| Variable | Default | Accepted values |
|---|---|---|
| `PORT` | `8080` | A number from 1 to 65535 |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `LOG_FORMAT` | `json` | `json` or `text` |
| `DRAIN_DELAY` | `0s` | A Go duration of 0 or more, for example `5s`. After SIGTERM, how long `/readyz` returns 503 while the server still serves, so the load balancer stops sending traffic. |
| `SHUTDOWN_TIMEOUT` | `20s` | A Go duration of 0 or more. How long in-flight requests get to finish after the drain. Keep `DRAIN_DELAY` plus `SHUTDOWN_TIMEOUT` below the Kubernetes grace period (30s by default). |

## Shutdown

On SIGINT or SIGTERM the gateway marks itself as draining (`/readyz` returns 503), waits `DRAIN_DELAY`, stops accepting connections, and gives in-flight requests up to `SHUTDOWN_TIMEOUT` to finish. A signal never cancels a request that is already running; only the timeout does.
