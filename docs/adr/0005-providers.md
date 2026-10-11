# 0005. Providers: Ollama, Groq, OpenAI, Anthropic

- Status: Accepted
- Date: 2026-10-10
- Decided by: Stephen Lwanga
- Implemented in: S1 (planned): Ollama, and one OpenAI-compatible adapter for Groq and OpenAI. S5 (planned): Anthropic.

## Context

The gateway routes each request to an upstream provider through an adapter behind one `Provider` interface. The first provider set decides whether that interface is proven general: an interface tested against providers that share one wire protocol can hide assumptions that break on the next protocol. Development and CI also need a way to run real model calls at low cost.

## Considered options

1. Ollama (local), Groq, OpenAI and Anthropic.
2. OpenAI-compatible providers only: Ollama, Groq and OpenAI.
3. One hosted aggregator that fronts many models behind one API.

## Pros and cons

| Option | Pros | Cons |
|---|---|---|
| Ollama, Groq, OpenAI, Anthropic | One local and three hosted providers. Two wire protocols: the OpenAI-compatible API (Ollama, Groq, OpenAI) and the Anthropic Messages API. Ollama runs models locally with no per-token cost. Groq has a free plan with rate limits. | Two adapters to build and test, with different streaming events and usage reporting. |
| OpenAI-compatible only | One adapter covers all three. Fastest to build. | The `Provider` interface is never tested against a second protocol, so its assumptions stay hidden. |
| Hosted aggregator | Many models with one integration. | Routing, failover and pricing move upstream, which is the gateway's own job. One external dependency for every request. |

## Decision outcome

Chosen: option 1.

- **Ollama** is the local provider. It supports a subset of the OpenAI API, including `/v1/chat/completions`. It runs on a developer machine with no API key.
- **Groq** and **OpenAI** share one OpenAI-compatible adapter. Groq's base URL is `https://api.groq.com/openai/v1`.
- **Anthropic** uses the Messages API (`POST /v1/messages`): the system prompt is a top-level `system` parameter, not a message role, and streaming uses its own event types. Usage in `message_delta` is cumulative.

The adapter seam is proven when one interface serves both protocols with the same contract tests: cancellation, a callback error, the first-byte timeout, the stall timeout and usage reporting.

## Consequences

- Good: the router (S4) can fail over between providers on different protocols, so failover is tested across real differences.
- Good: development runs against Ollama or Groq's free plan before it uses paid providers.
- Bad: each adapter maps its provider's finish reasons, usage fields and errors to the gateway's types and to the OpenAI envelope ([0002](0002-openai-compatible-api-and-error-envelope.md)).
- Bad: OpenAI reports streamed usage only when the request sets `stream_options.include_usage`, so the adapter must set it.

## Links

- Ollama OpenAI compatibility: https://docs.ollama.com/api/openai-compatibility
- Groq OpenAI compatibility: https://console.groq.com/docs/openai
- Groq rate limits and plans: https://console.groq.com/docs/rate-limits
- Anthropic Messages API: https://platform.claude.com/docs/en/api/messages
- OpenAI chat completions streaming: https://platform.openai.com/docs/api-reference/chat/create
