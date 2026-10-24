# AGENTS.md

Rules for anyone, human or agent, who changes this repo.

## What this is

llm-gateway is a multi-tenant, OpenAI-compatible LLM gateway in Go. The
design is in `docs/design.md` and the decisions are in `docs/adr/`.

## How work is done

- One branch and one pull request per milestone, named `feat/sNN-<name>`.
- Merge commits only. No squash, no rebase merge.
- The README and the pages in `docs/` are updated in the same pull request as
  the code they describe.

## Commits

- Conventional commits: `type(scope): subject`. No function names in the subject.
- No commit trailers, and no `Co-Authored-By`.
- One purpose per commit. A formatting change goes in its own commit.

## Pull requests

- **Title:** a plain sentence saying the outcome, for example "Stream chat
  completions from Groq, OpenAI and Ollama through one endpoint". Not
  `feat(...)` style. No file, variable, command or code names, and no
  contractions.
- **Body:** `.github/pull_request_template.md`: Summary, Test plan, Docs,
  Checklist.
  - Write it for someone who has not read the design. Say what changes and
    why in plain words. A milestone or decision number (S02, ADR 0016) is
    never the explanation.
  - **Write in ASD-STE100 Simplified Technical English.** At most 20 words in
    an instruction and 25 in a description. Active voice, present tense, one
    idea per sentence, articles kept. Use the same word for the same thing
    every time. No contractions, no em dashes and no en dashes.
  - **Name no identifiers in the prose:** no files, folders, packages, types,
    functions, variables, settings, tables, columns, migrations, milestone
    numbers or decision numbers. Say what the thing is: "the router", "the
    configuration document", "the routes table". An identifier appears only
    where a reader must type or find it, such as a command or a setting, and
    a body has at most one. Product names are fine: Go, OpenAI, Groq, Ollama,
    Postgres, OpenTelemetry.
  - Summary: one short paragraph, then one bullet per outcome, each opening
    with the outcome in bold.
  - Test plan: each line is a behavior that was checked, ticked when it
    passed. No test file paths. The result of the checks is one line of
    numbers: "The checks before every commit pass on Go 1.26.10: 183 tests
    and 224 subtests in 18 packages, 0 lint issues."
  - Before every `gh pr create` or `gh pr edit`, write the body to a file and
    run `go run ./tools/prbody <file>` from the repo root. Fix each problem it
    lists. After you post, read the live body back with `gh pr view` and
    check it again. The check cannot judge plain words or one idea per
    sentence, so read the body too. `tools/prbody/testdata/model.md` is the
    model.
  - No "Generated with" footer and no session link.

## Writing

- No em dashes in any file. Use a comma, a colon or a full stop.

## Code rules

- **Layout.** `cmd/<binary>` holds each entry point. `internal/<component>`
  holds one package per component (config, telemetry, provider, router, and
  so on), not one package per layer. `tools/<name>` holds a tool for working
  on the repo, which no image ships. An interface is defined in the package
  that uses it, not in the package that implements it.
- **Errors.** Every error response uses the OpenAI error shape:
  `{"error": {"message", "type", "param", "code"}}`. Packages return
  sentinel errors (`errors.Is`); only the HTTP edge maps an error to a
  status code and that shape.
- **Comments.** Comments say why, only where the code cannot. No concept
  explanations (those go in `docs/`), no restating the code. Exported doc
  comments are one line unless a contract needs more.
- **Secrets.** A struct that holds a `config.Secret` in an unexported field
  must implement `fmt.Formatter` and `slog.LogValuer`, because `fmt` prints
  unexported fields by reflection without calling their methods.
- **Test helpers.** Test helpers that import test-only dependencies (for
  example testcontainers) live in `_test.go` files. govulncheck scans every
  non-test package, so a helper package pulls those dependencies into the
  production scan.

## Checks before every commit

```bash
gofmt -l .        # must print nothing
go vet ./...
go test -race ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 run ./...   # the version CI pins
```
