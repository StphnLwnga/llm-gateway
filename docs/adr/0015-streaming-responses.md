# 0015. Streaming responses: status hold, heartbeats and a liveness watchdog

- Status: Accepted
- Date: 2026-10-11
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 1: 2823892, 3a3dc79, deade1b, 869776b, 02253be

## Context

An HTTP response sends its status line once, before the body. A streamed chat completion sends the body as tokens arrive, so the gateway must choose when to send the status. An upstream error that arrives before the status can still become a real status, such as 429 or 502. After the status, it can only be an event inside a 200 response.

Measurements in milestone 1 showed that the HTTP client's header timeout does not bound the time to the first token. Groq sends its headers 4 to 139 ms before the first token on long prompts, and 50 to 257 ms early on a reasoning model, whose reasoning streams first. Ollama sends its headers with the first token, after a model load of several seconds. A cold load or a long reasoning phase leaves the client with no bytes for seconds, and proxies close connections that stay idle too long.

Three decisions follow: when to send the status, how to detect a stream that has stopped while a reasoning model is still healthy, and how to serve a client that does not ask for a stream.

## Considered options

Decision 1, the status of a streamed response:

1. Commit 200 at once and report every upstream error as an event.
2. Hold the status until the first token or the first error, however long that takes.
3. Hold the status for up to `STATUS_HOLD` (3s). Then commit 200 and send SSE comment heartbeats until the first token.

Decision 2, liveness while a reasoning model reasons:

1. Adapters emit a progress event, with no content, for each reasoning step. The watchdog resets on any event.
2. The watchdog's deadlines apply to any bytes from the upstream, not to events.
3. Adapters forward reasoning as its own event field.

Decision 3, a request with `stream: false`:

1. Call `Provider.Chat`, a non-streamed upstream request.
2. Stream from the upstream through the same watchdog, and join the stream into one `chat.completion` response.

## Pros and cons

| Decision 1 | Pros | Cons |
|---|---|---|
| Commit at once | Simple. The client gets headers at once. | Every failure, including a 429 or an auth error in the first milliseconds, is a 200 with an error event. Clients, SDK retries, proxies and monitoring see success. |
| Hold until the first token | Every error before the first token gets a real status. | A cold load or a long reasoning phase sends no bytes, so proxy idle timeouts and client read timeouts can cut the connection. |
| Hold for up to 3s, then heartbeats | Most upstream errors (429, 5xx, auth, bad request) arrive within milliseconds and get a real status. A slow start gets 200 and heartbeats that keep the connection open. | An error after the hold is an event. Two code paths. |

| Decision 2 | Pros | Cons |
|---|---|---|
| Progress events | Explicit: each adapter knows its protocol. The watchdog measures model work. The watchdog stays a provider decorator with no access to the transport. | Every adapter must emit them. A new field in the event contract. |
| Deadlines on any upstream bytes | No adapter change. | The watchdog needs the transport, below the adapter. An upstream that sends keep-alive comments while stuck would never time out. |
| Reasoning as an event field | The most information. | Decides now whether clients see reasoning, which belongs to milestone 5, and OpenAI's chat completions shape has no such field. |

| Decision 3 | Pros | Cons |
|---|---|---|
| `Provider.Chat` | The upstream returns one body. | The header timeout then caps the total generation time on servers that send headers at the end. No liveness check. No time to first token for metrics. |
| Stream and join | One upstream path for both request types, with the same watchdog and metrics. A long answer has no total time limit. | The joined response must match OpenAI's non-stream shape exactly. A test compares it field by field with a recorded Groq non-stream response to the same request. The whole answer is held in memory until the end. |

## Decision outcome

Chosen: option 3 for decision 1, option 1 for decision 2, and option 2 for decision 3.

```text
streamed request:
  first token within STATUS_HOLD -> 200, then chunks, the finish chunk, the usage chunk, data: [DONE]
  upstream error first           -> a real status in the OpenAI error shape
  STATUS_HOLD passes             -> 200, ": heartbeat" every HEARTBEAT_INTERVAL until the first token
  error after the 200            -> data: {"error":{...}}, then the stream ends with no data: [DONE]

non-streamed request:
  upstream stream joined -> 200 chat.completion at the end, or a real status for any error
```

- **The watchdog** (`provider.Watchdog`) wraps every provider in `cmd/gateway`. The first event must arrive within `PROVIDER_FIRST_TOKEN_TIMEOUT` (60s), and each later event within `STREAM_STALL_TIMEOUT` (30s) of the one before. When a deadline passes, it cancels the upstream request and returns a provider error of kind `timeout`, which the client sees as 504 or an error event. A client disconnect stays a cancellation, not a timeout, so it never counts against a provider. Time spent writing to the client is not counted as upstream silence.
- **Progress events.** `Event.Progress` marks an event with no content. The OpenAI-compatible adapter emits one for each `delta.reasoning` (Groq), and the Ollama adapter for each `message.thinking`. The handler does not forward them; whether clients see reasoning is a milestone 5 decision.
- **The error event** is `data: {"error":{"message","type","param","code"}}` with no `event:` line. The OpenAI Python SDK raises `APIError` for a stream event with an `error` key (`src/openai/_streaming.py`, lines 92 to 106, in v3.29.0). No `data: [DONE]` follows it, so a cut answer never looks complete.
- **Status mapping.** A rate limit is 429 with the upstream `Retry-After`. An upstream auth failure is 502, because the credential is the gateway's. Another upstream 4xx is 400 with a fixed message. A 5xx, malformed body or connection error is 502. A timeout is 504. The upstream's own text goes only to the log.
- **Heartbeats** are SSE comment lines, which clients ignore. Each event write has its own deadline, cleared after the write, because over HTTP/2 a passed write deadline cannot be extended.

## Consequences

- Good: fast failures reach clients and SDK retries as real statuses.
- Good: a reasoning model or a cold local model does not trip the watchdog or a proxy's idle timeout.
- Good: a long non-streamed answer has no total time limit, and both request types report time to first token.
- Bad: an error after the hold reaches the client only as an event, which a client must handle.
- Bad: a non-streamed request holds the full answer in memory, and gets no partial answer on failure.

## Links

- openai-python, the stream loop: https://github.com/openai/openai-python/blob/v3.29.0/src/openai/_streaming.py#L92-L106
- WHATWG HTML, interpreting an event stream (comment lines): https://html.spec.whatwg.org/multipage/server-sent-events.html#event-stream-interpretation
- `net/http.ResponseController.SetWriteDeadline`: https://pkg.go.dev/net/http#ResponseController.SetWriteDeadline
- Related: [0005](0005-providers.md) (providers), [0012](0012-streaming-interface-callback.md) (the streaming interface)
- Code: `internal/provider/watchdog.go`, `internal/sse/sse.go`, `internal/httpapi/chat.go`
