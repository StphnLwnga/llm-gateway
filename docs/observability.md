# Observability

The gateway records traces and metrics with the OpenTelemetry SDK, and sends them over OTLP/HTTP to a collector. [ADR 0016](adr/0016-observability.md) gives the reasons. Export is off until `OTEL_EXPORTER_OTLP_ENDPOINT` is set. The [configuration page](configuration.md) lists the settings.

## Traces

Each HTTP request gets one server span. Each call to a provider gets a GenAI span under it, and the HTTP request to the provider gets a client span under that:

```
POST /v1/chat/completions          server
└── chat llama-3.1-8b-instant      client, GenAI
    └── HTTP POST                  client, HTTP
```

| Span | Name | Attributes |
|---|---|---|
| HTTP server | `{method} {route}`, for example `POST /v1/chat/completions` | `http.request.method`, `http.route`, `http.response.status_code`, `http.response.body.size`, `url.path`, `url.scheme`, `server.address`, `server.port`, `network.protocol.version`, `network.peer.address`, `network.peer.port`, `client.address`, `user_agent.original` |
| GenAI inference (one per provider call) | `chat {gen_ai.request.model}`, for example `chat llama-3.1-8b-instant` | See the next table. |
| HTTP client (to a provider) | `HTTP {method}`, for example `HTTP POST` | `http.request.method`, `http.response.status_code`, `url.full`, `server.address`, `server.port`, `network.protocol.version` |

The route is the pattern that matched, so an unknown path has the route `/`. A response of 500 or more marks the span as an error. A 4xx response does not, because the fault is the client's.

### GenAI span

The GenAI names follow the OpenTelemetry GenAI semantic conventions at commit [6fd0d76](https://github.com/open-telemetry/semantic-conventions-genai/tree/6fd0d763a092db245a54104fc1124031eb3c51d2/docs/gen-ai) ([ADR 0016](adr/0016-observability.md)). The first five attributes are set when the span starts, so a sampler can use them.

| Attribute | Value |
|---|---|
| `gen_ai.operation.name` | `chat` |
| `gen_ai.provider.name` | The configured provider: `groq`, `openai` or `ollama` |
| `gen_ai.request.model` | The upstream model from the routes table, not the alias the client sent |
| `server.address`, `server.port` | From the provider's base URL |
| `gen_ai.request.stream` | `true`. The chat endpoint streams from every provider, also for a non-streamed request |
| `gen_ai.response.model` | The model the provider says answered, cut to 256 bytes. On the span only, never on a metric (decision 11) |
| `gen_ai.response.finish_reasons` | For example `["stop"]`. `["error"]` when the answer did not finish: an upstream error, a cut stream or a cancel |
| `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` | As the provider reports them. The input count is left out when the provider does not report it |
| `gen_ai.response.time_to_first_chunk` | Seconds, when a chunk arrived |
| `error.type` | When the call failed; see [error types](#error-types) |
| `llm_gateway.provider.*_duration` | The provider's own timings, in seconds; see the next table |

The provider's own timings split the time to first chunk into the provider's queue, model load and prefill. The provider measures them inside its own servers, so a slow client does not change them. The GenAI conventions at the pinned commit have no names for them, so they use the `llm_gateway.provider` namespace (decision 18). They are on the span only, never on a metric. A timing that the provider does not report has no attribute, because 0 is a real value. OpenAI reports none.

| Attribute | Groq field (seconds) | Ollama field (nanoseconds) |
|---|---|---|
| `llm_gateway.provider.queue_duration` | `queue_time` | Not reported |
| `llm_gateway.provider.load_duration` | Not reported | `load_duration` |
| `llm_gateway.provider.prefill_duration` | `prompt_time` | `prompt_eval_duration` |
| `llm_gateway.provider.decode_duration` | `completion_time` | `eval_duration` |
| `llm_gateway.provider.total_duration` | `total_time`: prefill and decode, without the queue | `total_duration`: from the request's arrival in Ollama, so it includes the load |

Groq sends the usage in `usage` and in `x_groq.usage` of a streamed answer. The adapter reads `usage`, and `x_groq.usage` when `usage` is absent.

A failed call sets the span status to error, with the error type as its description. The provider's own message stays in the log. A cancelled call leaves the status unset, because the client chose to leave. The span records no prompt, completion or reasoning text, and no events.

The SDK keeps every span, and the collector decides which traces to keep. The local collector keeps a trace when one of these policies matches ([development](development.md#observability-stack)):

| Policy | Keeps |
|---|---|
| `errors` | Every trace with a span in error |
| `slow` | Every trace longer than 30 s |
| `slow-first-token` | Every trace with a GenAI span whose `gen_ai.response.time_to_first_chunk` is above 1 s |
| `baseline` | 10% of the rest, or `TRACE_SAMPLE_PERCENT` |

- **Why 1 s.** A warm local model sends its first chunk in about 0.45 s ([benchmarks](benchmarks.md#time-to-first-token-on-a-local-model)). Above 1 s means a model load or a queue, and the baseline would drop 90% of those traces ([ADR 0016](adr/0016-observability.md), decision 19).
- **Under overload.** Almost every request waits longer than 1 s for its first chunk. Thus, the number of kept traces rises with the load.
- **`TRACE_SAMPLE_PERCENT`.** An environment variable of the collector, not of the gateway. The default is 10. Set it to 100 to keep every trace for an experiment: `TRACE_SAMPLE_PERCENT=100 make up-obs`.

| Trace context | Behaviour |
|---|---|
| Incoming `traceparent` | The server span continues its trace ID, with the client's span as the parent. Its sampled flag is ignored: a request is recorded even when the flag is `00` ([ADR 0016](adr/0016-observability.md), decision 5). |
| No `traceparent` | The server span starts a new trace. |
| Request to a provider | Carries a `traceparent` with the same trace ID and the sampled flag `01`. |
| Baggage | Not read and not forwarded. |
| `/healthz` and `/readyz` | No span. Their metrics are recorded (decision 6). |

## Metrics

### HTTP metrics

The HTTP metrics follow the stable OpenTelemetry HTTP semantic conventions.

| Metric | Type | Unit | Attributes |
|---|---|---|---|
| `http.server.request.duration` | histogram | `s` | `http.request.method`, `http.route`, `http.response.status_code`, `url.scheme`, `server.address`, `server.port`, `network.protocol.name`, `network.protocol.version` |
| `http.server.request.body.size` | histogram | `By` | the same |
| `http.server.response.body.size` | histogram | `By` | the same |
| `http.client.request.duration` | histogram | `s` | `http.request.method`, `http.response.status_code`, `server.address` (the provider's host), `server.port`, `url.scheme`, `network.protocol.name`, `network.protocol.version` |
| `http.client.request.body.size` | histogram | `By` | the same |

The HTTP duration of a streamed request covers the whole stream. A stream that lasts longer than the largest HTTP bucket (10 s) counts only in the overflow bucket. The GenAI buckets reach 81.92 s.

### GenAI client metrics

Each provider call records these metrics. The histograms use the bucket boundaries that the conventions advise: 0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96 and 81.92 s.

| Metric | Type | Unit | Attributes |
|---|---|---|---|
| `gen_ai.client.inference.duration` | histogram | `s` | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `server.address`, `server.port`, and `error.type` when the call failed |
| `gen_ai.client.inference.time_to_first_chunk` | histogram | `s` | the same, without `error.type` |
| `gen_ai.client.inference.time_per_output_chunk` | histogram | `s` | the same, without `error.type` |
| `gen_ai.client.inference.usage.input_tokens` | counter | `{token}` | the same, without `error.type`, and `gen_ai.token.modality` |
| `gen_ai.client.inference.usage.output_tokens` | counter | `{token}` | the same as the input tokens |

- A chunk is an upstream event with content or reasoning. An event with only the usage or the finish reason is not a chunk. A heartbeat is not a chunk: the gateway writes it, not the provider.
- The duration and the time to first chunk start when the gateway calls the provider. The duration equals the GenAI span's duration.
- The time per output chunk is recorded for each chunk after the first. It leaves out the time the gateway takes to hand the previous chunk to the client, so a slow client does not make the provider look slow.
- Chunk timings are recorded for every call, because the gateway always streams from the provider.
- Each provider call records its own values. When failover arrives, each attempt records its own.
- The token counters record every call, failed ones too. Reported tokens have the modality `text`. A call with no usage records 0 under `unknown`. When the provider does not report the prompt count, no input tokens are recorded for that call.

### Gateway metrics

These metrics describe a chat request as the client sees it. Failures are counted by how the request ended, not by its status: a stream that sent 200 can still fail.

| Metric | Type | Unit | Attributes | Records |
|---|---|---|---|---|
| `llm_gateway.chat.time_to_first_token` | histogram | `s` | `gen_ai.provider.name` | From the request's arrival to the first content written to the client. Streamed requests only. Same buckets as the GenAI metrics |
| `llm_gateway.chat.stream.commits` | counter | `{stream}` | `llm_gateway.commit.reason`: `first_token`, `hold_elapsed` or `error` | Each streamed request that sent a status, by what sent it |
| `llm_gateway.chat.stream.truncations` | counter | `{stream}` | `gen_ai.provider.name`, `error.type` | Each stream cut by an upstream error after its 200 |
| `llm_gateway.chat.client_disconnects` | counter | `{request}` | `llm_gateway.stage`: `before_commit` or `after_commit` | Each request whose client left before the answer ended |
| `llm_gateway.provider.watchdog.timeouts` | counter | `{timeout}` | `gen_ai.provider.name`, `llm_gateway.deadline`: `first_token` or `stall` | Each provider call that a liveness deadline ended |

Prometheus stores the names with underscores and a unit suffix, for example `gen_ai_client_inference_duration_seconds` and `llm_gateway_chat_stream_commits_total`.

### Error types

`error.type` has a fixed list of values:

| Value | The call ended because |
|---|---|
| `rate_limited` | the provider answered 429 |
| `auth` | the provider answered 401 or 403 |
| `bad_request` | the provider answered another 4xx |
| `timeout` | the provider answered 408 or 504, the connection timed out, or a watchdog deadline passed |
| `upstream_unavailable` | the provider answered 5xx, could not be reached, or sent a stream that broke, did not parse or carried an error event |
| `_OTHER` | of an error that is not a provider error |
| `cancelled` | the client left, so the gateway cancelled the call. On the GenAI duration only |

### Bounded attributes

Every attribute has a bounded set of values:

| Attribute | Values |
|---|---|
| `http.route` | The registered patterns, and `/` for an unknown path |
| `http.request.method` | The standard methods, and `_OTHER` for any other method |
| `server.address`, `server.port` | Fixed by the gateway, not taken from the request's `Host` header. On GenAI metrics, from the provider's base URL |
| `gen_ai.provider.name` | The configured providers |
| `gen_ai.request.model` | The upstream models in the routes table |
| `error.type` | The [error types](#error-types) |
| `gen_ai.token.modality` | `text` and `unknown` |
| `llm_gateway.*` | The fixed lists in the gateway metrics table |

No metric carries a tenant, a request ID, the response model, a provider timing or a model alias from the request.

## Dashboards

Two Grafana dashboards chart these metrics: one for the gateway and one for the collector. The [development page](development.md#dashboards) lists them and explains how to change them.

## Logs

A log line written inside a request carries the `trace_id` and `span_id` of the server span. The request line (`chat completion`) and the `upstream error` line both have them, so a log search leads to the trace.
