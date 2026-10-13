# Configuration

Settings come from environment variables. The gateway and the admin CLI read the same settings. The gateway reports every invalid setting together at startup, then exits with code 1.

Provider keys go in a `.env` file at the repo root, which Compose passes to the gateway. The [development guide](development.md) gives the local setup.

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
| `OTEL_EXPORTER_OTLP_ENDPOINT` | none | An http or https URL of an OTLP/HTTP receiver, for example `http://otel-collector:4318`. When it is set, the gateway exports traces and metrics there (`/v1/traces`, `/v1/metrics`). When it is not set or empty, telemetry is off and costs nothing. |
| `OTEL_SERVICE_NAME` | `llm-gateway` | The `service.name` resource attribute. Read by the OpenTelemetry SDK. |
| `OTEL_RESOURCE_ATTRIBUTES` | none | Extra resource attributes as `key=value` pairs, separated by commas, for example `deployment.environment.name=staging`. Read by the OpenTelemetry SDK. |
| `OTEL_METRIC_EXPORT_INTERVAL` | `60000` | How often the gateway exports metrics, in milliseconds. `make up-obs` sets `15000`. Read by the OpenTelemetry SDK. |
| `ROUTES_REFRESH_INTERVAL` | `10s` | A Go duration above 0. How often the gateway reloads the routes table into memory. A change made with `gatewayctl routes` takes effect within one interval. |

The OpenTelemetry SDK also reads the other standard `OTEL_EXPORTER_OTLP_*` settings, such as headers, timeout and compression. The sampler and the propagator are fixed, so `OTEL_TRACES_SAMPLER` and `OTEL_PROPAGATORS` have no effect. The SDK keeps every span, the collector samples, and the propagator is W3C TraceContext ([ADR 0016](adr/0016-observability.md)).
