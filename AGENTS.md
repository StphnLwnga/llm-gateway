# AGENTS.md

Rules for anyone, human or agent, who changes this repo.

## What this is

llm-gateway is a multi-tenant, OpenAI-compatible LLM gateway in Go. The
design is in `docs/design.md` and the decisions are in `docs/adr/`.

## How work is done

- One branch and one pull request per slice, named `feat/sNN-<name>`.
- A slice is built in subslices. Each subslice is one commit, or a few.
- Agents write the code. Stephen reviews each subslice and merges.
- Merge commits only. No squash, no rebase merge.
- The README is updated in the same pull request as the code it describes.

## Commits

- Conventional commits: `type(scope): subject`. No function names in the subject.
- No commit trailers, and no `Co-Authored-By`.
- One purpose per commit. A formatting change goes in its own commit.

## Writing

- No em dashes in any file. Use a comma, a colon or a full stop.

## Code rules

- **Layout.** `cmd/<binary>` holds each entry point. `internal/<component>`
  holds one package per component (config, telemetry, provider, router, and
  so on), not one package per layer. An interface is defined in the package
  that uses it, not in the package that implements it.
- **Errors.** Every error response uses the OpenAI error shape:
  `{"error": {"message", "type", "param", "code"}}`. Packages return
  sentinel errors (`errors.Is`); only the HTTP edge maps an error to a
  status code and that shape.
- **Comments.** Comments say why, only where the code cannot. No concept
  explanations (those go in `docs/`), no restating the code. Exported doc
  comments are one line unless a contract needs more.

## Checks before every commit

```bash
gofmt -l .        # must print nothing
go vet ./...
go test -race ./...
```
