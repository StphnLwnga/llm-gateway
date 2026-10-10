# llm-gateway

A multi-tenant, OpenAI-compatible LLM gateway in Go: each tenant calls one API with its own key, and the gateway routes the request to Ollama, Groq, OpenAI or Anthropic with rate limits, budgets, usage metering and tracing.

Status: S0 (walking skeleton) in progress.

Design: [docs/design.md](docs/design.md).
