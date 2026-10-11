# 0013. Provider credentials: platform keys and tenant keys, the tenant key wins

- Status: Accepted
- Date: 2026-10-11
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 1: platform keys, e61965a, d3994f9. Milestone 3 (planned): tenant keys.

## Context

Every upstream call needs a provider API key. The key decides three things: who pays the provider, whose rate limits apply, and whose agreement with the provider covers the data. Some tenants want to start without a provider account. Others already have a provider contract, with negotiated rate limits, prices or data-processing terms, and want their requests to run under it.

## Considered options

1. Platform keys only: one key per provider, owned by the gateway operator.
2. Tenant keys only: each tenant brings its own key for each provider it uses.
3. Both. A tenant key for a provider is used when the tenant has one; otherwise the platform key is used.

## Pros and cons

| Option | Pros | Cons |
|---|---|---|
| Platform keys only | One key per provider to manage. A tenant can call any configured provider at once. | Every tenant shares one set of provider rate limits, so one busy tenant can cause 429s for the others. A tenant cannot use its own contract or data terms. The operator pays for every request. |
| Tenant keys only | Each tenant has its own provider limits and terms, and pays the provider directly. | A tenant cannot make a first call until it has a provider account and has stored a key. Local development and tests also need a key per tenant. |
| Both, tenant key wins | Works out of the box with platform keys. A tenant with its own contract uses it. Rate limits for those tenants are separate at the provider. | Two credential paths to build and test. Each usage record must say which key paid, so billing and cost reports are correct. |

## Decision outcome

Chosen: option 3. For each request, the gateway uses the tenant's key for that provider when one exists, and the platform key otherwise. If neither exists, that provider is not available to the tenant.

```text
request for provider P from tenant T
  ├── T has a key for P (M3)   -> use T's key
  ├── platform key for P set   -> use the platform key
  └── neither                  -> P is not available to T
```

- **Platform keys (built).** `GROQ_API_KEY` and `OPENAI_API_KEY` come from the environment as `config.Secret` values. A provider whose key is not set is disabled, and the gateway logs that at startup. `cmd/gateway` builds one adapter per enabled provider into a registry keyed by provider name, and routes refer to providers by that name. `GROQ_BASE_URL` and `OPENAI_BASE_URL` point an adapter at a compatible endpoint, such as the fake provider in tests.
- **Tenant keys (milestone 3).** They are stored per tenant and provider in `provider_credentials`, with AES-GCM envelope encryption. The associated data includes the tenant ID, so a ciphertext copied to another tenant's row fails to decrypt ([design.md](../design.md), section 6).
- **Keys never appear in output.** `config.Secret` prints a placeholder through `fmt`, `log/slog` and `encoding/json`. `fmt` does not call those methods for an unexported struct field, so the adapter, which holds its key in an unexported field, has its own `Format` and `LogValue`. Tests check the error, `%+v`, `%#v` and the JSON log output for the key.

## Consequences

- Good: development and CI run against the fake provider or one platform key, with no tenant setup.
- Good: a tenant that uses its own key is isolated from other tenants' rate limits at the provider.
- Bad: usage events must carry the credential source (platform or tenant) from milestone 6, or cost reports will charge the wrong party.
- Bad: milestone 3 must decide what happens when a tenant key fails with 401 or 403. Using the platform key instead would bill the operator for a tenant that chose its own key, and could send data outside the tenant's provider agreement.

## Links

- The Twelve-Factor App, config in the environment: https://12factor.net/config
- OWASP, Secrets Management Cheat Sheet: https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html
- Groq rate limits: https://console.groq.com/docs/rate-limits
- Design: [docs/design.md](../design.md), section 6
- Code: `internal/config/config.go`, `cmd/gateway/providers.go`, `internal/provider/openaicompat/openaicompat.go`
