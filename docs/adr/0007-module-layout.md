# 0007. Module layout: cmd/, internal/ by component, consumer-side interfaces

- Status: Accepted
- Date: 2026-10-10
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 0

## Context

The module will hold three binaries (`gateway`, `worker`, `gatewayctl`) and a test binary (`fakeprovider`), plus about fifteen components: config, the HTTP edge, providers, the router, tenancy, the key vault, rate limits, caches, guardrails, usage, storage and telemetry. The layout decides where a change lands, which packages may import which, and how a test replaces a dependency.

## Considered options

1. `cmd/<binary>` for each entry point, `internal/<component>` with one package per component, and each interface defined in the package that uses it.
2. Layered packages: `handlers/`, `services/`, `repositories/`, `models/`.
3. A flat module: one package, or public packages under `pkg/`.

## Pros and cons

| Option | Pros | Cons |
|---|---|---|
| `cmd/` and `internal/` by component | A change to one component stays in one package. `internal/` stops other modules from importing the packages, so their APIs can change freely. Consumer-side interfaces keep each interface small and keep the import arrow pointing from the user to its dependency. | Package boundaries need judgement. A shared type needs a clear owner. |
| Layered packages | Familiar from MVC frameworks. | One feature touches every layer. `models/` becomes a shared package that everything imports, and import cycles follow. |
| Flat or `pkg/` | Few decisions at the start. | No boundaries as the code grows. `pkg/` publishes an API that the project does not intend to support. |

## Decision outcome

Chosen: option 1.

- `cmd/gateway/main.go` is the composition root. `run` reads the configuration, builds the parts and connects them. No other package reads the environment or chooses a concrete type.
- An interface is defined where it is used. `internal/httpapi` defines `Checker` for readiness checks. `internal/store/postgres` provides `Pinger` with a `Check` method and does not import `httpapi`. Go interfaces are satisfied implicitly, so the compiler checks the fit in `run`.

```text
cmd/gateway ──> internal/httpapi (defines Checker)
           └──> internal/store/postgres (Pinger satisfies Checker, imports nothing from httpapi)
```

## Consequences

- Good: a test passes a fake `Checker` to `httpapi.New` with no database.
- Good: the drift test (`readme_test.go`) checks that each `cmd/` and `internal/` folder is listed in the README, so the layout stays documented.
- Bad: an interface can be repeated in two consumers. That is accepted while each consumer needs a different method set.

## Links

- Go modules layout: https://go.dev/doc/modules/layout
- Go Code Review Comments, interfaces: https://go.dev/wiki/CodeReviewComments#interfaces
- Code: `cmd/gateway/main.go`, `internal/httpapi/server.go`, `internal/store/postgres/postgres.go`
