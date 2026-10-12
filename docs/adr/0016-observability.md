# 0016. Observability: the OpenTelemetry SDK, no tenant label on metrics, OTLP to a collector, tail sampling

- Status: Accepted
- Date: 2026-10-12
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 2 (in progress)

## Context

The gateway needs traces, RED metrics and the GenAI latency and token metrics, and log lines that link to traces. It is multi-tenant, so every signal could carry a tenant ID. It runs on Compose, kind, EKS and GKE Autopilot ([0006](0006-deploy-targets.md)), so the backend differs per target. A streamed request can last minutes, and most requests succeed quickly, so keeping every trace costs storage for little value.

Four decisions follow: which labels metrics carry, which library records the signals, how signals leave the process, and where traces are sampled.

Two facts constrain the metric names:

- The GenAI semantic conventions are pinned to commit [6fd0d76](https://github.com/open-telemetry/semantic-conventions-genai/tree/6fd0d763a092db245a54104fc1124031eb3c51d2/docs/gen-ai) of `open-telemetry/semantic-conventions-genai`. At that commit, every GenAI metric has stability `development`, and the repository has no releases and no tags. Names can change, so the pin records which version the code follows.
- The gateway uses the client inference metrics (`gen_ai.client.inference.duration`, `gen_ai.client.inference.time_to_first_chunk`, `gen_ai.client.inference.time_per_output_chunk`, `gen_ai.client.inference.usage.*`), not `gen_ai.server.*`. The gateway is a client of its providers: it measures the time from its request to the provider's answer. The `gen_ai.server.*` metrics belong to a model server that runs the model.

## Decision 1: label budget

Each label multiplies the number of time series. The duration metrics advise 14 bucket boundaries, so one histogram has 15 buckets plus a sum and a count: 17 series per label combination. Take 3 providers, 10 models and 6 error types: none, plus the five provider error kinds of [0012](0012-streaming-interface-callback.md). One histogram is then 17 x 3 x 10 x 6 = 3,060 series, about 3,100. Three GenAI histograms are about 9,200 series. A tenant label with 1,000 tenants multiplies that to about 9.2 million.

| Option | Pros | Cons |
|---|---|---|
| A: tenant label on every metric | Per-tenant dashboards and alerts straight from the metrics backend. | About 9.2 million series at 1,000 tenants. Prometheus memory grows with active series, so cost grows with the tenant count, not with traffic. |
| B: no tenant label on metrics; the tenant goes on spans and log lines | The series count stays near 9,200 for any tenant count. Per-tenant questions use traces, logs, or the usage ledger (milestone 6), which is exact and billable. | No per-tenant latency panel from metrics alone. A per-tenant view needs a trace or log query. |
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

Chosen: C. The SDK sampler is `ParentBased(AlwaysOn)`, so the gateway records and exports every span and leaves the decision to the collector. An incoming `traceparent` that says "not sampled" is still honoured, so an upstream caller's decision holds.

## Consequences

- Good: the metric series count does not grow with the number of tenants.
- Good: metrics and traces come from one SDK and share the request context.
- Bad: the gateway binary grows by about 8 MB, and the compressed image by about 2.8 MB.
- Good: the gateway code has one export path, and the backend is a collector setting.
- Good: every error and slow request keeps its full trace.
- Bad: per-tenant latency needs a trace or log query, not a metrics panel.
- Bad: the collector is on the telemetry path. When it is down, the SDK drops what it cannot send, and the gateway logs the export failures.
- Bad: tail sampling needs memory in the collector, and a trace-aware load balancer when there is more than one collector.
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
- Related: [0006](0006-deploy-targets.md) (deploy targets), [0012](0012-streaming-interface-callback.md) (provider error kinds)
