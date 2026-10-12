# 0012. Streaming interface: a callback per event, the adapter owns the body

- Status: Accepted
- Date: 2026-10-11
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 1: 0e90219, 370e908, 57c4204

## Context

Every provider adapter turns an upstream HTTP response into a sequence of events: text deltas, then the usage and the finish reason. Callers consume that sequence in different ways. The chat handler writes each delta to the client as it arrives. The router (milestone 4) watches for the first delta, because it can fail over to another provider only before content has reached the client. The upstream response body holds an open connection until it is closed, and an unclosed body leaks the connection.

The interface decides who closes that body, how a caller stops a stream early, and how an error reaches the caller.

## Considered options

1. A callback: `Stream(ctx, Request, func(Event) error) error`. The adapter calls the function once per event and closes the body before it returns.
2. A pull iterator: `Stream` returns a value with `Next`, `Event`, `Err` and `Close`, like `sql.Rows`.
3. A range function: `Stream` returns `iter.Seq2[Event, error]`, consumed with `for ev, err := range`.

## Pros and cons

| Option | Pros | Cons |
|---|---|---|
| Callback | The adapter opens and closes the body in one function, so a `defer` covers every path, and the contract suite checks it once per adapter. An error from the callback is returned unchanged, so the caller can tell its own stop from an upstream failure. A slow callback stops the adapter from reading, so back-pressure reaches the upstream through TCP. | The caller's per-event code runs inside the adapter's loop. Merging two streams needs a goroutine and a channel. |
| Pull iterator | The caller controls the loop. A familiar shape in Go (`sql.Rows`, `bufio.Scanner`). | Every caller must call `Close` on every path. A missed `Close` leaks the connection, and the router's failover path, which abandons a stream after an error before the first delta, is where it is easiest to miss. The `bodyclose` linter does not follow a custom type. |
| `iter.Seq2` | `range` syntax, and `break` stops the stream. | An iterator that calls `yield` again after it returned false panics at runtime ("range function continued iteration after function for loop body returned false"), so a bug in an adapter's loop crashes the request instead of returning an error. The error travels with each element, and the loop body can ignore it. |

## Decision outcome

Chosen: option 1, the callback, in `internal/provider`.

```go
type Provider interface {
	Chat(ctx context.Context, req Request) (Response, error)
	Stream(ctx context.Context, req Request, fn func(Event) error) error
}
```

- **The contract.** `Stream` calls `fn` once per event, in order, and closes the upstream body before it returns, on every path. An error from `fn` stops the stream and is returned unchanged (`errors.Is`). When `ctx` ends, the error wraps `ctx.Err()`.
- **Upstream errors are typed.** Every upstream failure is a `*provider.Error` with the provider name, the status code, a kind (`rate_limited`, `auth`, `bad_request`, `upstream_unavailable`, `timeout`) and `Retry-After` when the provider sent it. The router (milestone 4) finds it with `errors.As` and decides whether to retry, fail over or give up, without parsing provider messages. A caller whose own context ended gets `ctx.Err()`, not a `*provider.Error`, so a client disconnect never counts against a provider.
- **The HTTP client has no overall timeout.** `http.Client.Timeout` covers the whole response, so it would cut off a long stream. `PROVIDER_CONNECT_TIMEOUT` bounds the connect, and `PROVIDER_FIRST_BYTE_TIMEOUT` bounds the wait for the response headers (`Transport.ResponseHeaderTimeout`). A stalled stream is cut by the stall timeout in the chat handler (milestone 1).
- **One contract suite.** `internal/provider/providertest` runs every case against each adapter, with a fake upstream that speaks the adapter's wire format: deltas in order, the final usage and finish reason, cancellation before the first byte and mid-stream, the callback error, the first-byte timeout, and 429 with `Retry-After`, 401, 500 and a malformed chunk mapped to their kinds. A tracking `RoundTripper` checks that the adapter used the client it was given and closed every body before returning. The suite runs against a reference adapter, and it fails when that adapter skips `Body.Close`, ignores `ctx` or swallows the callback error.
- **The interface lives with its types.** [0007](0007-module-layout.md) puts an interface in the package that uses it. Here the router, the chat handler and the contract suite all need the same method set, and every adapter must satisfy exactly that set, so one shared definition in `internal/provider` is the contract.

## Consequences

- Good: no caller has to close anything, so the router can abandon a stream on any path without a leak.
- Good: either other shape can be built on top of the callback when a caller needs it: a range function wraps the callback, and `iter.Pull2` turns that into a pull iterator.
- Bad: a caller that wants to stop early returns an error from the callback, then has to recognise that error with `errors.Is` to tell it from a failure.
- Bad: optional request fields are pointers, because zero is a valid temperature, top-p and seed, so every adapter checks for nil before it writes a field.

## Links

- Go, range over function types: https://go.dev/blog/range-functions
- `iter` package, including `Pull2`: https://pkg.go.dev/iter
- `net/http.Response` (the caller must close the body): https://pkg.go.dev/net/http#Response
- `net/http.Transport.ResponseHeaderTimeout`: https://pkg.go.dev/net/http#Transport
- RFC 9110, section 10.2.3, `Retry-After`: https://www.rfc-editor.org/rfc/rfc9110.html#section-10.2.3
- Code: `internal/provider/provider.go`, `internal/provider/errors.go`, `internal/provider/client.go`, `internal/provider/providertest/providertest.go`
