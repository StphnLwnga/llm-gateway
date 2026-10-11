# 0002. OpenAI-compatible API and error envelope

- Status: Accepted
- Date: 2026-10-10
- Decided by: Stephen Lwanga
- Implemented in: S0 (b): the error envelope, `GET /v1/models` stub. S1 (planned): `POST /v1/chat/completions`.

## Context

Clients of the gateway already speak the OpenAI API: the OpenAI SDKs, and tools that accept an OpenAI base URL. The gateway must accept those clients with no code change other than the base URL and the key. Clients also parse error bodies: an SDK reads `error.message`, `error.type` and `error.code` to build its exception. A different error shape makes every gateway error look like an unknown failure to the client.

The error edge has a second concern. An internal error chain can hold a database host, a SQL statement, a file path or an upstream provider's error body. None of that may reach a client.

## Considered options

1. OpenAI-compatible routes and the OpenAI error envelope for every error.
2. A gateway-specific API with its own request and error schema.
3. OpenAI-compatible routes, with errors as RFC 9457 problem details (`application/problem+json`).

## Pros and cons

| Option | Pros | Cons |
|---|---|---|
| OpenAI routes and envelope | Existing clients work by changing the base URL. SDK exception types work for gateway errors. One shape to test. | The gateway follows OpenAI's schema as it changes. Fields such as `param` are often `null`. |
| Own API | Full control of the schema. | Every client needs new code or a new SDK. Adoption cost is on every caller. |
| OpenAI routes, RFC 9457 errors | A standard error format with a registered media type. | OpenAI SDKs do not parse problem details, so clients see a generic error. Two shapes to document: success follows OpenAI, failure does not. |

## Decision outcome

Chosen: option 1. Every error response is `{"error":{"message","type","param","code"}}`.

- Packages return sentinel errors (`errors.Is`). Only the HTTP edge (`internal/httpapi/errors.go`) maps an error to a status, a `type` and a message.
- The client message comes only from an `apiError` that carries the exact public text and unwraps to the sentinel (`publicError(err, msg)`). It never comes from `err.Error()`, because any layer can add internal detail to a wrapped chain.
- Every 5xx returns the fixed message "The server had an error while processing your request." The full error chain goes to the log with the method, path and status.
- An unknown route returns 404 with the same body OpenAI returns, for example `{"error":{"message":"Invalid URL (GET /v1/nope)","type":"invalid_request_error","param":null,"code":null}}`. A known path with the wrong method returns 405 with an `Allow` header, in the same envelope.

## Consequences

- Good: an OpenAI SDK pointed at the gateway raises its normal exception types for gateway errors.
- Good: a test asserts the exact 404 body, and a mutation check (message taken from `err.Error()` again) failed 4 tests, so the public and internal split is covered.
- Bad: the 405 message is the gateway's own text. OpenAI's 405 body is not verified.
- Bad: provider errors (S1, S4, S5) must be translated into this envelope, including Anthropic's error types, so the mapping table grows with each adapter.

## Links

- OpenAI error codes: https://platform.openai.com/docs/guides/error-codes
- OpenAI community forum, the "Invalid URL" 404 body: https://community.openai.com/t/invalid-request-error-in-chatgpt-api/81213
- RFC 9457, Problem Details for HTTP APIs: https://www.rfc-editor.org/rfc/rfc9457
- Code: `internal/httpapi/errors.go`, `internal/httpapi/server.go`
