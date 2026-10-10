# llm-gateway

A multi-tenant, OpenAI-compatible LLM gateway in Go: each tenant calls one API with its own key, and the gateway routes the request to Ollama, Groq, OpenAI or Anthropic with rate limits, budgets, usage metering and tracing.

Status: S0 (walking skeleton) in progress.

Design: [docs/design.md](docs/design.md).

## Endpoints

| Method and path | What it does |
|---|---|
| `GET /healthz` | Liveness. `200 {"status":"ok"}` while the process serves HTTP. It never checks a dependency. |
| `GET /readyz` | Readiness. `200 {"status":"ready"}` when every readiness check passes; `503` while the server drains or when a check fails. |
| `GET /v1/models` | The OpenAI model list. A stub for now: `{"object":"list","data":[]}`. |

Every error uses the OpenAI error shape. An unknown route returns `404`, and a known path with the wrong method returns `405` with an `Allow` header:

```json
{"error":{"message":"not found: invalid URL (GET /v1/nope)","type":"invalid_request_error","param":null,"code":"not_found"}}
```

A `500` never returns the internal error text; the gateway logs it instead.
