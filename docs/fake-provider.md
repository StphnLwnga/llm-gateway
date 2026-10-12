# Fake provider

The fake provider serves a fake OpenAI chat completions API for local tests and load tests. Tests run the same handler in the test process. The fake has its own image target, so the production image does not contain it. Its output is deterministic: the answer is a fixed word sequence, and the usage counts match the tokens sent.

```bash
docker compose -f deploy/compose.yaml --profile loadtest up -d fakeprovider   # a plain `up` does not start it
curl -N localhost:8081/v1/chat/completions \
  -d '{"model":"fake?tokens=3&tpot=50ms","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

| It serves | Detail |
|---|---|
| `POST /v1/chat/completions` | Streamed or not. `stream_options.include_usage` adds the final usage chunk. |
| `GET /v1/models` | Lists one model, `fake` |
| Setting | `PORT`, default `8081` |
| Shutdown | On SIGINT or SIGTERM, open requests get 5 seconds before the fake closes them |

## Fault modes

The model name carries directives after a `?`, for example `fake?ttfb=2s&tokens=50&tpot=20ms`. The gateway sends the upstream model name unchanged. Thus, a client can choose the behaviour for each request. An unknown or invalid directive returns 400 in the OpenAI error shape.

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

| Limit | Value |
|---|---|
| Duration | 0 to 10m |
| Count | 0 to 100000 |
| `stall_after`, `drop_after`, `malformed_after` | At most one of them. Its count must not be more than `tokens`. |
| Client disconnect | Ends every delay |

## Timing example

```text
fake?first_token=300ms&tokens=3&tpot=50ms

t=0       200, text/event-stream headers
t=300ms   role chunk, then "The"
t=350ms   " quick"
t=400ms   " brown", then the stop chunk and [DONE]

fake?tokens=5&drop_after=2:     role, "The", " quick", then the connection closes
fake?tokens=5&stall_after=2:    role, "The", " quick", then nothing until the client leaves
```
