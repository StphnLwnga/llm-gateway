# Observability

The gateway records traces and metrics with the OpenTelemetry SDK, and sends them over OTLP/HTTP to a collector. [ADR 0016](adr/0016-observability.md) gives the reasons. Export is off until `OTEL_EXPORTER_OTLP_ENDPOINT` is set. The [configuration page](configuration.md) lists the settings.

## Traces

Each HTTP request gets one server span.

| Span | Name | Attributes |
|---|---|---|
| HTTP server | `{method} {route}`, for example `POST /v1/chat/completions` | `http.request.method`, `http.route`, `http.response.status_code`, `http.response.body.size`, `url.path`, `url.scheme`, `server.address`, `server.port`, `network.protocol.version`, `network.peer.address`, `network.peer.port`, `client.address`, `user_agent.original` |

The route is the pattern that matched, so an unknown path has the route `/`. A response of 500 or more marks the span as an error. A 4xx response does not, because the fault is the client's.

The SDK keeps every span, and the collector decides which traces to keep. The gateway does not read an incoming `traceparent` header yet, so every request starts a new trace.

## Metrics

The HTTP server metrics follow the stable OpenTelemetry HTTP semantic conventions.

| Metric | Type | Unit | Attributes |
|---|---|---|---|
| `http.server.request.duration` | histogram | `s` | `http.request.method`, `http.route`, `http.response.status_code`, `url.scheme`, `server.address`, `server.port`, `network.protocol.name`, `network.protocol.version` |
| `http.server.request.body.size` | histogram | `By` | the same |
| `http.server.response.body.size` | histogram | `By` | the same |

Every attribute has a bounded set of values:

| Attribute | Values |
|---|---|
| `http.route` | The registered patterns, and `/` for an unknown path |
| `http.request.method` | The standard methods, and `_OTHER` for any other method |
| `server.address`, `server.port` | Fixed by the gateway, not taken from the request's `Host` header |

No metric carries a tenant, a request ID or a model alias from the request.

The duration of a streamed request covers the whole stream. A stream that lasts longer than the largest bucket (10 s) counts only in the overflow bucket.

## Logs

A log line written inside a request carries the `trace_id` and `span_id` of the server span. The request line (`chat completion`) and the `upstream error` line both have them, so a log search leads to the trace.
