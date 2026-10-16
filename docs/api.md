# API

## Endpoints

| Method and path | What it does |
|---|---|
| `GET /healthz` | Liveness. `200 {"status":"ok"}` while the process serves HTTP. It never checks a dependency. |
| `GET /readyz` | Readiness. `200 {"status":"ready"}` when every readiness check passes; `503` while the server drains, when Postgres does not answer, or until the routes table has loaded once. |
| `GET /v1/models` | Needs an API key. The OpenAI model list: every alias in the routes table that has at least one enabled route with an enabled provider. `created` is the earliest route's creation time; `owned_by` is `llm-gateway`. |
| `POST /v1/chat/completions` | Needs an API key. OpenAI chat completions, streamed (SSE) or not. `model` is a route alias. See Chat completions below. |

## Authentication

Every `/v1/` request needs an API key in the `Authorization` header with the `Bearer` scheme, as the OpenAI SDKs send it. `/healthz` and `/readyz` need no key. `gatewayctl keys create` makes a key ([development](development.md#tenants-and-api-keys)).

```bash
curl localhost:8080/v1/models -H 'Authorization: Bearer gw_...'
```

A key is `gw_`, 43 random base62 characters (256 bits), and a 6-character CRC32 checksum of those characters: 52 characters in all. The gateway rejects a key with a wrong length, a character outside base62 or a wrong checksum before it reads the database. The database stores only the SHA-256 of the key and its first 8 characters after `gw_`.

A missing header, another scheme, a malformed key, a wrong checksum, an unknown or revoked key, and a key of a disabled tenant all get the same response, so the response never says which check failed. The gateway log records the reason.

```text
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Bearer

{"error":{"message":"Incorrect API key provided. Send the key in the Authorization header with the Bearer scheme.","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}
```

When the gateway cannot check a key, for example when Postgres is down, the response is a 500 `server_error`, not a 401.

The gateway keeps an accepted key in memory for `AUTH_CACHE_TTL`, 30 s by default ([configuration](configuration.md)). The time counts from the start of the lookup that accepted the key.

- **Revocation.** After a revoke or a tenant disable, a new request with the key can still pass until that time ends, and never after it. A request that the key authenticated before the revoke, such as a long stream, runs to its end.
- **Rejections.** A rejected key is not kept in memory, so a new key works on its first request.
- **Postgres down.** A key in memory still works until its time ends. After that, its requests get the 500.

`gatewayctl keys list` shows when each key was last used. The gateway saves these times once a minute and when it shuts down, so a time can be up to a minute later than the request. A gateway that stops without a shutdown, for example when it is killed, loses the uses since its last save.

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

Every response carries `X-Request-Id` and `X-LLM-Gateway-Provider`. The gateway writes one log line per request. The line has the tenant and key IDs, the provider, the status, the time to first token, the usage, and whether the stream was cut short.

```bash
curl -N localhost:8080/v1/chat/completions -H 'Authorization: Bearer gw_...' \
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
