# 0016. Observability: the OpenTelemetry SDK, no tenant label on metrics, OTLP to a collector, tail sampling

- Status: Accepted
- Date: 2026-10-12
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 2 (in progress)

## Context

The gateway needs traces, RED metrics and the GenAI latency and token metrics, and log lines that link to traces. It is multi-tenant, so every signal could carry a tenant ID. It runs on Compose, kind, EKS and GKE Autopilot ([0006](0006-deploy-targets.md)), so the backend differs per target. A streamed request can last minutes, and most requests succeed quickly, so keeping every trace costs storage for little value.

Eighteen decisions follow. Four are about the signals: which labels metrics carry, which library records them, how they leave the process, and where traces are sampled. Two are about requests: how an incoming trace context is treated, and whether health probes are traced. Twelve are about the GenAI span and metrics of each provider call, and the gateway's own chat metrics.

Two facts constrain the metric names:

- The GenAI semantic conventions are pinned to commit [6fd0d76](https://github.com/open-telemetry/semantic-conventions-genai/tree/6fd0d763a092db245a54104fc1124031eb3c51d2/docs/gen-ai) of `open-telemetry/semantic-conventions-genai`. At that commit, every GenAI metric has stability `development`, and the repository has no releases and no tags. Names can change, so the pin records which version the code follows.
- The gateway uses the client inference metrics (`gen_ai.client.inference.duration`, `gen_ai.client.inference.time_to_first_chunk`, `gen_ai.client.inference.time_per_output_chunk`, `gen_ai.client.inference.usage.*`), not `gen_ai.server.*`. The gateway is a client of its providers: it measures the time from its request to the provider's answer. The `gen_ai.server.*` metrics belong to a model server that runs the model.

## Decision 1: label budget

Each label multiplies the number of time series. The duration metrics advise 14 bucket boundaries, so one histogram has 15 buckets plus a sum and a count: 17 series per label combination. Take 3 providers and 10 models. At the pinned commit, only `gen_ai.client.inference.duration` carries `error.type`. It has 8 values: none, the five provider error kinds of [0012](0012-streaming-interface-callback.md), `_OTHER` for an error that is not a provider error, and `cancelled` (decision 14).

| Metric | Label combinations | Series |
|---|---|---|
| `gen_ai.client.inference.duration` | 3 providers x 10 models x 8 error types | 17 x 240 = 4,080 |
| `gen_ai.client.inference.time_to_first_chunk` | 3 x 10 | 510 |
| `gen_ai.client.inference.time_per_output_chunk` | 3 x 10 | 510 |
| `gen_ai.client.inference.usage.input_tokens` and `.output_tokens` (counters) | 3 x 10 x 2 modalities (`text`, `unknown`), one series each | 120 |
| `llm_gateway.chat.time_to_first_token` (decision 7) | 3 providers | 51 |
| The four gateway counters (decision 17) | at most 29 | 29 |
| Total | | about 5,300 |

`server.address` and `server.port` have one value per provider, so they do not multiply. A tenant label with 1,000 tenants multiplies the total to about 5.3 million.

| Option | Pros | Cons |
|---|---|---|
| A: tenant label on every metric | Per-tenant dashboards and alerts straight from the metrics backend. | About 5.3 million series at 1,000 tenants. Prometheus memory grows with active series, so cost grows with the tenant count, not with traffic. |
| B: no tenant label on metrics; the tenant goes on spans and log lines | The series count stays near 5,300 for any tenant count. Per-tenant questions use traces, logs, or the usage ledger (milestone 6), which is exact and billable. | No per-tenant latency panel from metrics alone. A per-tenant view needs a trace or log query. |
| C: a tenant label on counters only, capped to the top N tenants plus "other" | Per-tenant request and token rates for the largest tenants. | The cap needs a ranking that changes over time, so a tenant moves between its own series and "other". Counters still multiply by N. Two sources of per-tenant usage (metrics and ledger) can disagree. |

Chosen: B. Metrics answer "is the system healthy", across providers and models. Per-tenant usage comes from the ledger, which milestone 6 builds for billing anyway. Traces and logs carry the tenant ID for debugging one tenant's requests.

## Decision 2: instrumentation library

| Option | Pros | Cons |
|---|---|---|
| A: the OpenTelemetry SDK for metrics and traces | One SDK. Metrics and traces share the request context, so an exemplar links a histogram bucket to a trace. The GenAI semantic conventions are defined in OpenTelemetry terms. The backend changes through collector configuration. | The OpenTelemetry metrics SDK is younger and larger than the Prometheus client. Metric names are translated for Prometheus. |
| B: the Prometheus client (`client_golang`) for metrics, and OpenTelemetry for traces | The Prometheus client is mature, and the metric names are exactly what Prometheus stores. | Two libraries and two sets of labels to keep consistent. Exemplars need the trace ID copied by hand. The trace exporter brings most of the OpenTelemetry weight anyway. |
| C: the Prometheus client only, with no traces | Smallest and simplest. | No traces. A slow stream cannot be broken down into its stages, and log lines have no trace to link to. |

Chosen: A. The gateway needs traces, and Prometheus has no tracing. One SDK keeps metrics and traces on the same request context.

The cost, measured on 2026-10-12 with the linux/arm64 build:

| Build | Gateway binary | Image, compressed |
|---|---|---|
| Before milestone 2 | 11.01 MB | 8.17 MB |
| With `client_golang` v1.25.0 (one histogram and its HTTP handler) | 13.17 MB | not built |
| With the OpenTelemetry SDK v1.47.0 and both OTLP/HTTP exporters | 19.14 MB | 11.00 MB |

Metric names are translated when Prometheus stores them: by default, dots become underscores, and a unit suffix is added (`gen_ai.client.inference.duration` becomes `gen_ai_client_inference_duration_seconds`). Prometheus is still the metrics store, the query engine and the alert source.

## Decision 3: export path

| Option | Pros | Cons |
|---|---|---|
| A: OTLP push from the gateway to an OpenTelemetry Collector, for all signals; the collector forwards metrics to Prometheus and traces to Tempo | One exporter in the code. Sampling, redaction and routing are central, in the collector. The backend changes with no code change. | One more component to run. Metrics depend on the collector being up; the SDK drops data it cannot send. |
| B: Prometheus scrapes a `/metrics` endpoint; traces go by OTLP | Scraping is the Prometheus default, and a scrape failure is visible as a down target. | Two export paths in the code. Metric relabelling and redaction happen per backend. A cloud backend needs its own scrape setup. |

Chosen: A. Both target clouds accept OTLP. AWS CloudWatch has OTLP endpoints for metrics, logs and traces, and they require the collector's SigV4 authentication extension. Google Cloud has the Telemetry (OTLP) API. So the same gateway build sends to Prometheus and Tempo locally, and to the cloud backend on EKS or GKE, with only collector configuration changing.

The exporter uses OTLP over HTTP with protobuf, which the OTLP exporter specification makes the default protocol. It needs no HTTP/2 connection to the collector. In OpenTelemetry Go v1.47.0, the HTTP exporters still link the gRPC packages. Their configuration packages and the OTLP protobuf service definitions import them. Thus, this choice does not make the binary smaller.

## Decision 4: sampling

| Option | Pros | Cons |
|---|---|---|
| A: keep every trace | Nothing is lost. | Storage grows with traffic, and most traces are fast successes that nobody reads. |
| B: head sampling in the gateway, for example 10% | Cheap: an unsampled request records no spans. | The decision is made before the request runs, so it drops 90% of errors and slow requests, which are the traces worth keeping. |
| C: tail sampling in the collector: keep every error, every slow request and 10% of the rest | Keeps the interesting traces and bounds storage. The policy changes in the collector, with no deploy of the gateway. | The collector holds spans in memory until it decides (30 s by default). Every span of a trace must reach the same collector instance, so more than one collector needs a trace-ID-aware load balancer in front. The gateway exports every span. |

Chosen: C. The SDK sampler is `AlwaysOn`, so the gateway records and exports every span and leaves the decision to the collector. Decision 5 explains why it is not `ParentBased`.

## Decision 5: incoming trace context

A client can send a W3C `traceparent` header. Its sampled flag says whether the client recorded its own span.

| Option | Pros | Cons |
|---|---|---|
| A: trust the incoming context fully (`ParentBased`) | The client's sampling decision holds across services, as the W3C model intends. | A client that sends flag `00` switches off recording in the gateway. Its errors, such as a 502 from a provider, never reach the collector. |
| B: continue the trace ID and ignore the sampled flag | One trace from the client through the gateway to the provider. The gateway records every request, so the collector's tail sampling sees every error. | The gateway records spans that the client chose not to record. The collector bears that cost and drops what its policy does not keep. |
| C: start a new root span, and add a span link to the client's context | The client cannot influence the gateway's traces at all. | A trace stops at the gateway. Following a request from a product to a provider needs a link lookup, not one trace. |

Chosen: B. Tail sampling in the collector makes the keep decision, so a client must not be able to switch off recording, errors included. Products that call through the gateway keep one trace from end to end. The SDK sampler is `AlwaysOn`. A span still takes the incoming context as its parent, so the trace ID continues. The request to the provider carries the trace context with the sampled flag set, because the gateway always records.

The propagator is W3C TraceContext only, with no Baggage. Nothing in the gateway reads baggage yet. A client's baggage would also be forwarded to third-party providers, with whatever data the client put in it.

## Decision 6: health and readiness probes

Kubernetes calls `/healthz` and `/readyz` every few seconds on every pod.

| Option | Pros | Cons |
|---|---|---|
| A: spans and metrics for probes | Nothing is special-cased. | A probe trace has no debugging value. Probes produce more traces than real traffic on an idle pod, and tail sampling keeps 10% of them. |
| B: metrics only, no spans | A failing readiness check stays visible in the metrics, by route and status. No probe traces to store. | Two handler paths in the instrumentation: probes go through one whose tracer records nothing. |
| C: neither spans nor metrics (the OpenTelemetry HTTP handler's filter) | The simplest setting. | A 503 from `/readyz` during an outage or a drain disappears from the metrics. |

Chosen: B. Probe traces add noise and storage cost but no debugging value, while a readiness failure must stay visible in the metrics.

## Decision 7: time to first chunk

The conventions time `gen_ai.client.inference.time_to_first_chunk` "from when the client issues the generation request to when the first chunk is received in the response stream". They do not say whether a chunk of reasoning counts. A client of the gateway sees only content: the adapters turn reasoning into progress events and do not forward it.

| Option | Pros | Cons |
|---|---|---|
| A: the conventions' metric only: any chunk with content or reasoning, timed from the request to the provider | Follows the definition and measures the provider. Matches the watchdog: a reasoning event also meets the first-token deadline ([0015](0015-streaming-responses.md)). | On a reasoning model, the client sees nothing while the model reasons. A recorded Groq `gpt-oss` stream had 29 reasoning chunks before any content. This time is then far shorter than the client's wait. |
| B: the conventions' name, but only content counts, timed from the request's arrival at the gateway | Shows what the client feels. | The name no longer means what the conventions define, so a dashboard built for the conventions misreads it. It mixes gateway time and provider time, and with failover, failed attempts too. |
| C: A, plus a gateway histogram `llm_gateway.chat.time_to_first_token`: from the request's arrival to the first content written to the client, with the provider as its only label | Each number has one meaning: the provider's responsiveness, and the client's wait. | 51 more series. Two time-to-first-token numbers, which dashboards must name clearly. |

Chosen: C. The gateway histogram is recorded for streamed requests only. A non-streamed answer is written whole at the end, and `http.server.request.duration` already measures that. The `ttft_ms` field of the request log keeps its meaning: from the request's arrival to the first content.

## Decision 8: heartbeats

The gateway writes an SSE comment to a stream that waits for its first token past the status hold ([0015](0015-streaming-responses.md)). An adapter skips the SSE comments that an upstream sends.

| Option | Pros | Cons |
|---|---|---|
| A: a heartbeat is not a chunk, and is not counted | The GenAI metrics measure the provider, and a heartbeat never comes from it. The `hold_elapsed` commit count (decision 17) shows how often a client received heartbeats. | No direct count of heartbeats sent. |
| B: also count heartbeats sent | Shows the volume of keep-alive traffic. | Little value next to the gateway's time to first token, which measures the wait directly. One more instrument. |
| C: treat an upstream SSE comment as progress | A provider that sends comments while it works would not trip the stall deadline. | Changes the watchdog's behaviour, not only its measurement. |

Chosen: A.

## Decision 9: one span per attempt

The conventions say that a GenAI span covers a logical operation, with every automatic retry of it. Failover (milestone 4) moves a request to another provider and model, so one span cannot describe both targets.

| Option | Pros | Cons |
|---|---|---|
| A: one GenAI span and one set of GenAI metrics for each attempt at a target (a provider and a model) | Each attempt has its own clock, provider, model and `error.type`, so latency goes to the provider that caused it. Retries to the same target stay inside its span. Failover adds a loop around the wrapper and changes nothing inside it. | The view of a whole request comes from the server span, `http.server.request.duration` and the gateway's time to first token, not from one GenAI span. |
| B: one GenAI span per request, with each attempt as a span event | One span gives the model time of a request. | The provider and model attributes can name only one target. Metrics cannot put latency on the right provider. Failover would move the span boundary. |
| C: A, plus an `INTERNAL` parent span per request that groups its attempts | A trace shows the attempts side by side under one span. | One more span per request before there is more than one attempt. |

Chosen: A. Milestone 4 decides whether to add the parent span of C.

## Decision 10: span parents

| Option | Pros | Cons |
|---|---|---|
| A: the server span, then the GenAI span (kind `CLIENT`), then the HTTP client span | The conventions use `CLIENT` when the call goes over an instrumented protocol such as HTTP. The GenAI span's context reaches the adapter, so the existing HTTP client span becomes its child with no change. The HTTP span keeps the status, the URL and the connection timing. | Two nested `CLIENT` spans per call. |
| B: the GenAI span as `INTERNAL` | One `CLIENT` span per call. | Against the conventions, which keep `INTERNAL` for a model in the same process. A service map built from `CLIENT` spans loses the edge to the provider. |
| C: no HTTP client span; the transport keeps its metrics | One span fewer per call. | The trace loses the HTTP status, the URL and the connection timing. |

Chosen: A.

## Decision 11: the response model

`gen_ai.response.model` is the one attribute whose value an upstream chooses. The fake provider answers `fake` to a request for `fake?tokens=5`, and OpenAI answers with dated model names. At the pinned commit, the conventions do not require it on metrics. `gen_ai.request.model` comes from the routes table, so it is bounded already.

| Option | Pros | Cons |
|---|---|---|
| A: on the span only, cut to 256 bytes; never on a metric | No risk to the series count. | When a provider moves a name to a new model version, traces show it and metric labels do not. |
| B: on metrics only when it equals a routed upstream model, otherwise `_OTHER` | Bounded by the routes table. | Dated names never match, so OpenAI would show only `_OTHER`. |
| C: the first 3 values for each provider and request model in a process, then `_OTHER` | Bounded at 3 per route. | Each process keeps its own list, so replicas and restarts disagree, and the result depends on the order of requests. |

Chosen: A. C can come later with no name change.

## Decision 12: chunk timings for non-streamed requests

The gateway serves a non-streamed request from an upstream stream, so the same watchdog applies to both ([0015](0015-streaming-responses.md)). The conventions record chunk timings for streaming calls only.

| Option | Pros | Cons |
|---|---|---|
| A: chunk timings for every upstream call, with `gen_ai.request.stream` set to true | True from the gateway's side: every call to a provider streams. A provider's latency does not depend on what the client asked for. | The metrics do not show which client requests streamed. The request log does. |
| B: chunk timings only when the client asked for a stream | Matches the client's view. | A flag from the client decides whether provider metrics exist, and some provider calls have no chunk timings. |

Chosen: A.

## Decision 13: slow clients and the time per chunk

The adapter cannot read the next chunk while the handler writes the current one to the client.

| Option | Pros | Cons |
|---|---|---|
| A: from the arrival of one chunk to the arrival of the next, as the conventions word it | The literal reading. | A slow client makes the provider look slow. |
| B: leave out the time spent handing a chunk to the handler, as the watchdog does | The metric measures the provider, not the gateway's clients. | Data that waited in the receive buffer during a slow write arrives at once, so the gap after a slow write can look near zero. |

Chosen: B.

## Decision 14: cancelled calls

A client that leaves cancels the provider call. The conventions time a call until "the response is fully received or the operation ends with an error or cancellation", and they ask for the finish reason `error` for an answer that did not finish.

| Option | Pros | Cons |
|---|---|---|
| A: record the duration with `error.type` `cancelled`, span status unset, and finish reasons `["error"]` | A cut partial answer does not count as a success. A cancel is the client's choice, not a fault, so it does not raise the error rate of traces. | One more `error.type` value on the duration (8 instead of 7). |
| B: record no duration for a cancelled call | No extra value. | The count of calls is lower than the real one, and the conventions still ask for the call's usage record. |

Chosen: A.

## Decision 15: an unknown prompt count

Ollama leaves out the prompt count when the prompt came from its cache. The adapter then marks the count unknown.

| Option | Pros | Cons |
|---|---|---|
| A: record no input tokens for that call | No false zero in the token totals. | The input total does not include that call's tokens. |
| B: record 0 under the modality `unknown`, as for a call with no usage | Every call has an input record. | 0 is too low, so a cost estimate from the metric is too low. |

Chosen: A. The output tokens are still recorded. The usage ledger (milestone 6) is the source for billing.

## Decision 16: token histograms

| Option | Pros | Cons |
|---|---|---|
| A: the usage counters only | Token totals and rates, for cost. 120 series. | No percentiles of tokens per call. |
| B: also `gen_ai.client.inference.operation.input_tokens` and `.output_tokens` | p95 and p99 of tokens per call, which find outlier prompts. | About 3,060 series each, because they carry `error.type`. |

Chosen: A. The reasoning and cache counters need the adapters to read more of the usage object, so they are not recorded yet.

## Decision 17: gateway counters

A streamed request can fail after its status 200 was sent. A status code therefore cannot count these failures.

| Option | Pros | Cons |
|---|---|---|
| A: four counters, each with one question: `llm_gateway.chat.stream.commits` by what sent the status, `llm_gateway.provider.watchdog.timeouts` by provider and deadline, `llm_gateway.chat.stream.truncations` by provider and `error.type`, and `llm_gateway.chat.client_disconnects` by whether the status was sent | Each counter has one or two attributes and is clear alone. | A ratio between two of them needs a query that joins two metrics. |
| B: one counter, `llm_gateway.chat.outcomes`, with the provider, the stream flag, what sent the status, the outcome and `error.type` | One query gives every ratio. About 150 series. | Most combinations of attributes cannot occur, which makes panels harder to read. |

Chosen: A. The handler records three of them from the same record that writes the request's log line, so the log and the counters agree. The watchdog counter is recorded for each provider call, so after failover it counts each attempt.

## Decision 18: the provider's own timings

Groq reports its queue, prompt, completion and total time in the usage object, in seconds. Ollama reports its load, prompt evaluation, evaluation and total durations, in nanoseconds. OpenAI reports none. The GenAI registry at the pinned commit has no attribute for them; its only time attribute, `gen_ai.response.time_to_first_chunk`, is measured by the client.

| Option | Pros | Cons |
|---|---|---|
| A: do not record them | No code. | A slow first chunk looks the same whether the provider queued the request, loaded the model, or read a long prompt. |
| B: histograms of them, in a later milestone | Percentiles per provider and model on dashboards. | About 510 series for each timing, 2,550 for the five. |
| C: attributes of the GenAI span, `llm_gateway.provider.*_duration` in seconds | The time to first chunk splits into queue, model load and prefill, at no series cost. The provider measures them inside its own servers, so they are the only speed signal per provider that client backpressure does not change. | No percentiles from metrics: a trace query gives them. Each provider bounds its total differently, so totals do not compare across providers. |

Chosen: C. A timing that the provider does not report has no attribute, because 0 is a real value.

## Consequences

- Good: the metric series count does not grow with the number of tenants.
- Good: metrics and traces come from one SDK and share the request context.
- Bad: the gateway binary grows by about 9.8 MB (11.01 MB to 20.84 MB), and the compressed image by about 3.1 MB (8.17 MB to 11.30 MB), measured on 2026-10-12 with the linux/arm64 build.
- Good: the gateway code has one export path, and the backend is a collector setting.
- Good: every error and slow request keeps its full trace, even when the client sent flag `00`.
- Good: a request keeps one trace ID from the client through the gateway to the provider.
- Bad: per-tenant latency needs a trace or log query, not a metrics panel.
- Bad: the collector is on the telemetry path. When it is down, the SDK drops what it cannot send, and the gateway logs the export failures.
- Bad: tail sampling needs memory in the collector, and a trace-aware load balancer when there is more than one collector.
- Good: each provider call has a GenAI span between the server span and the HTTP client span, with no prompt or completion text.
- Good: a stream cut after its status 200 is counted by error type, so a failure that the status code hides stays visible.
- Bad: there are two times to first token: the conventions' one per provider call, and the gateway's one per request. Dashboards must say which one they show.
- Bad: a provider's change of model version shows in traces, not in metric labels.
- Good: a trace shows how much of a slow first chunk was the provider's queue, model load or prefill.
- Bad: the GenAI conventions are in development. A rename upstream means a code change and a dashboard change, and the pin records which version the code follows.

## Links

- GenAI semantic conventions at the pinned commit: https://github.com/open-telemetry/semantic-conventions-genai/tree/6fd0d763a092db245a54104fc1124031eb3c51d2/docs/gen-ai
- OTLP exporter configuration (default protocol): https://opentelemetry.io/docs/specs/otel/protocol/exporter/
- OpenTelemetry SDK environment variables: https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/
- Trace SDK samplers (`ParentBased`): https://opentelemetry.io/docs/specs/otel/trace/sdk/
- Tail sampling processor: https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/tailsamplingprocessor
- AWS CloudWatch OTLP endpoints: https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch-OTLPEndpoint.html
- AWS X-Ray OTLP endpoint: https://docs.aws.amazon.com/xray/latest/devguide/xray-opentelemetry.html
- Google Cloud Telemetry (OTLP) API: https://cloud.google.com/stackdriver/docs/reference/telemetry/overview
- Collector SigV4 authentication extension: https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/extension/sigv4authextension
- Prometheus instrumentation practices (label cardinality): https://prometheus.io/docs/practices/instrumentation/
- W3C Trace Context (the `traceparent` header and its flags): https://www.w3.org/TR/trace-context/
- Related: [0006](0006-deploy-targets.md) (deploy targets), [0012](0012-streaming-interface-callback.md) (provider error kinds), [0015](0015-streaming-responses.md) (status hold, heartbeats and watchdog)
