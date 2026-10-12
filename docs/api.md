# API

## Endpoints

| Method and path | What it does |
|---|---|
| `GET /healthz` | Liveness. `200 {"status":"ok"}` while the process serves HTTP. It never checks a dependency. |
| `GET /readyz` | Readiness. `200 {"status":"ready"}` when every readiness check passes; `503` while the server drains, when Postgres does not answer, or until the routes table has loaded once. |
| `GET /v1/models` | The OpenAI model list: every alias in the routes table that has at least one enabled route with an enabled provider. `created` is the earliest route's creation time; `owned_by` is `llm-gateway`. |
| `POST /v1/chat/completions` | OpenAI chat completions, streamed (SSE) or not. `model` is a route alias. See Chat completions below. |

## Errors

Every error uses the OpenAI error shape. An unknown route returns 404. A known path with the wrong method returns 405 with an `Allow` header.

```json
{"error":{"message":"Invalid URL (GET /v1/nope)","type":"invalid_request_error","param":null,"code":null}}
```

This 404 body matches the OpenAI 404 body. A 500 returns a fixed message, never the internal error text. The gateway logs the full error.

## Chat completions

The endpoint takes the OpenAI request shape. Thus, the OpenAI SDKs work unchanged when their base URL points at the gateway. The model name is a route alias. An unknown alias returns 404 with code `model_not_found`. The gateway uses the first route of the alias. Failover across routes comes in milestone 4.

| Request field | Support |
|---|---|
| `messages` | String content, or arrays of text parts |
| `stream`, `stream_options.include_usage` | Supported |
| `temperature`, `top_p`, `stop`, `seed` | Supported |
| `max_completion_tokens` or `max_tokens` | Without a limit, the route's default applies. A limit above the route's maximum returns 400. |
| `n` | 1 only |
| `tools`, `response_format`, `logprobs` | 400 until they are supported |
| Other fields | Ignored |

### Streaming and the status

A streamed request holds its HTTP status for up to `STATUS_HOLD` (3s):

```text
first token within the hold  -> 200, then the tokens
an upstream error first      -> a real status: 429 (with Retry-After), 502, 504 or 400
the hold passes              -> 200, then ": heartbeat" comments every HEARTBEAT_INTERVAL until the first token
an error after the 200       -> data: {"error":{...}} and the stream ends, with no data: [DONE]
```

A watchdog cuts a stream in two cases:

| Case | Limit |
|---|---|
| No event arrives | `PROVIDER_FIRST_TOKEN_TIMEOUT` |
| The stream goes silent | `STREAM_STALL_TIMEOUT` |

Reasoning models keep the watchdog alive while they think. A client that disconnects cancels the upstream request.

### Non-streamed requests

The gateway serves a non-streamed request from an upstream stream too. It joins the stream into one `chat.completion` response. The status goes out only at the end. Thus, every error gets a real status, and a long answer has no total time limit.

### Headers and logs

Every response carries `X-Request-Id` and `X-LLM-Gateway-Provider`. The gateway writes one log line per request. The line has the provider, the status, the time to first token, the usage, and whether the stream was cut short.

```bash
curl -N localhost:8080/v1/chat/completions \
  -d '{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

### Upstream errors

Each upstream error maps to a fixed public message. The upstream's own text goes only to the log.

| Upstream | Client gets |
|---|---|
| 429 rate limited | `429`, with the upstream `Retry-After` |
| 401 or 403 | `502` (the gateway's credential, not the client's) |
| other 4xx | `400` "The upstream provider rejected the request." |
| 5xx, malformed body, connection error | `502` |
| first-byte, first-token or stall timeout | `504` |

## Routes

A route maps a model alias to one provider and its upstream model. The alias is the name that a client sends as the model. One alias can have several routes, and the lowest priority number comes first. Routes live in the `routes` table. The admin CLI manages them:

```bash
gatewayctl routes add --alias chat --provider groq --upstream-model openai/gpt-oss-20b --priority 10 \
  --input-price 0.05 --output-price 0.08 --default-max-tokens 512 --max-max-tokens 4096
gatewayctl routes add --alias chat --provider ollama --upstream-model llama3.2:3b --priority 20
gatewayctl routes list
gatewayctl routes disable 1
```

| Rule | Detail |
|---|---|
| Prices | US dollars per million tokens, 0 or more, with at most six decimal places. Stored exactly: `numeric` in Postgres, integer micro-dollars in Go. |
| Provider | `groq`, `openai` or `ollama` only |
| Token limits | `--max-max-tokens` is not below `--default-max-tokens` |

The gateway keeps the enabled routes in memory. It reloads them every `ROUTES_REFRESH_INTERVAL`. Thus, a request never waits on Postgres to find its route. A change made with the admin CLI takes effect within one interval.

```text
gatewayctl routes add ──> Postgres routes table
                              │  every ROUTES_REFRESH_INTERVAL
                              v
gateway: in-memory snapshot (swapped atomically) ──> /v1/models, routing
         a failed reload keeps the last good snapshot
```

The gateway skips a route whose provider is not enabled: no key, or no Ollama URL. It logs the skipped routes once each time that set changes. `/readyz` returns 503 until the routes table has loaded once. After that, a failed reload does not make the pod unready. The pod keeps serving the last good snapshot.
