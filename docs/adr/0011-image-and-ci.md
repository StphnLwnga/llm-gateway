# 0011. Distroless static image, CI lint set, SHA-pinned actions, one test job

- Status: Accepted
- Date: 2026-10-11
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 0: 4a27ba3 (the image), a1d9a11, 6386ff3, 23f1210, 1973a1c (CI and the toolchain).

## Context

The gateway ships as a container image and is checked by CI on every pull request and every push to `main`. Four choices: the runtime base image, the static analysis set (D1), how workflow actions are referenced (D2), and how the Postgres integration tests run in CI (D3).

## Runtime image

| Option | Pros | Cons |
|---|---|---|
| `gcr.io/distroless/static-debian12:nonroot` | No shell, no package manager, no libc. Holds CA certificates and timezone data. Runs as a non-root user. Fewer packages to scan and patch. | No shell for debugging inside the container; an ephemeral debug container is needed. |
| Alpine | Small, with a shell and `apk`. | More packages to patch. musl libc, which a CGO-off binary does not need. |
| `scratch` | Smallest possible. | No CA certificates, timezone data or non-root user unless copied in by hand. |

Chosen: distroless static, non-root. Both binaries build with `CGO_ENABLED=0 -trimpath -ldflags="-s -w"` in a two-stage Dockerfile. Measured: 30.9 MB on disk, 7.35 MB compressed. Cold start to the first `/readyz` 200 took 174 to 185 ms over three runs.

## D1: static analysis

| Option | Pros | Cons |
|---|---|---|
| golangci-lint v2, `standard` set only | Low noise. | Misses security findings (gosec), wrong error comparisons (errorlint), unclosed bodies (bodyclose) and calls without a context (noctx). |
| `standard` plus gosec, errorlint, bodyclose, noctx, revive, misspell | Each added linter targets a defect class this code has: HTTP bodies, contexts, wrapped errors, integer conversions, unused parameters. | Some findings need a decision per site. |
| `default: all` with exclusions | Widest coverage. | Many style linters conflict or add noise. The exclusion list becomes the real configuration. |

Chosen: `standard` plus the six. Settings in `.golangci.yml`:

- `max-issues-per-linter: 0` and `max-same-issues: 0`, so every finding is shown. The default of 3 hides repeats of the same message.
- gosec G101 (hardcoded credentials) is excluded only in `_test.go` files. The tests use fake connection strings with a password, to prove the password is never printed.
- Unchecked errors are decided per site, not with the `std-error-handling` preset. A `Close` error on a writer can mean lost data, and a preset cannot tell a writer from a reader. `srv.Close` after a timed-out `Shutdown` is joined into the returned error. `db.Close` after a successful migration is logged as a warning. The rest are explicit discards (`_ =`).
- gosec G115 (integer overflow on conversion) is fixed with a range check in `postgres.Open`, not a `nolint` comment, because `Open` is exported ([0009](0009-pgx-pool-with-lazy-connect.md)).

## D2: action references

| Option | Pros | Cons |
|---|---|---|
| Tag (`@v7`) | Readable. Gets fixes automatically. | The tag can be moved to different code by the action's owner or by an attacker who takes over the account. The next run executes that code with the job's token. |
| Full commit SHA with a `# vX.Y.Z` comment, plus Dependabot | Immutable: GitHub's guide calls a full-length SHA "currently the only way to use an action as an immutable release". Dependabot opens a pull request for each new version, and CI runs on it. | A pinned SHA never updates by itself; Dependabot pull requests must be reviewed. |
| Fork or vendor each action | Full control. | Every update is manual work. |

Chosen: full SHA pins with Dependabot weekly for GitHub Actions, Go modules and the Docker base images, each with a 7-day cooldown. For an annotated tag, pin the commit the tag points to (`refs/tags/vX.Y.Z^{}`), not the tag object.

The same reasoning covers the base images and the update timing:

- **Base images by digest.** A digest is immutable. A tag such as `golang:1.26.9-bookworm` can be pushed again with other contents, and the next build would use them. The Dockerfile pins each base image as `tag@sha256:...`: the tag stays for readability, and the digest decides what is pulled. Dependabot's `docker` ecosystem updates the digest.
- **A 7-day cooldown.** Dependabot opens a version update only when the version is at least 7 days old. Malicious releases are often found and pulled within hours: litellm 1.82.7 and 1.82.8 on PyPI were live on 2026-03-24 for about 40 minutes before PyPI quarantined them. Security updates are not delayed, because the cooldown does not apply to them. The cost: normal fixes arrive 7 days later.

## D3: integration tests in CI

| Option | Pros | Cons |
|---|---|---|
| One `go test -race ./...` job, testcontainers on the runner's Docker | The same test code runs locally and in CI. Each integration test starts its own Postgres container, so tests share no state. | Slower: the Postgres package took 13.7 s in CI. Docker is needed locally. |
| Separate unit and integration jobs | Faster feedback from unit tests. | Two jobs to keep in step; a package can drift into the wrong one. |
| Skip integration tests in CI (`-short`) | The fastest CI. No Docker on the runner. | CI no longer runs the tests for the migration lock, readiness against a real Postgres, or password redaction in `postgres.Open`. Those behaviours would be checked only on a developer machine with Docker. |
| GitHub service containers for Postgres | Postgres starts before the job. | The tests connect differently in CI and locally. One shared database for all packages. |

Chosen: one race job with testcontainers. The tests skip without Docker locally, and fail when `CI` is set, so CI can never pass by skipping them.

## Other CI settings

- Jobs: lint (`gofmt -l`, `go mod tidy -diff`, `go vet`, golangci-lint v2.14.0), test, vuln (`govulncheck` v1.9.0), image (`docker build`, no push). They run in parallel. The first green run took about 70 s wall clock.
- `permissions: contents: read`, so the job token cannot push.
- `concurrency` with `cancel-in-progress`, so a new push to the same ref cancels the old run.
- **Toolchain pin.** `go.mod` has `go 1.26.0` and `toolchain go1.26.9`, and the Docker builder is `golang:1.26.9-bookworm`. govulncheck found 12 reachable standard-library vulnerabilities in go1.26.4, all fixed by 1.26.9. setup-go reads the `toolchain` line, and with `GOTOOLCHAIN=auto` an older local Go switches to 1.26.9.
- `readme_test.go` fails when a setting, a route or a package folder is missing from the docs. Settings are checked in `docs/configuration.md`, routes in `docs/api.md`, and packages in the README project layout. Each item must start its own line, so a mention in another row does not count.

## Consequences

- Good: a red check blocks a merge once the `main` ruleset requires the four jobs.
- Good: a vulnerable dependency or toolchain fails CI only when the code can reach the vulnerable function.
- Bad: debugging inside the runtime container needs an ephemeral debug container.
- Bad: Dependabot pull requests add review work every week, including one more for the base images.
- Bad: with the cooldown, non-security updates arrive 7 days after their release.

## Links

- distroless: https://github.com/GoogleContainerTools/distroless
- golangci-lint linters: https://golangci-lint.run/docs/linters/
- gosec rules: https://github.com/securego/gosec/blob/master/RULES.md
- GitHub, secure use of Actions: https://docs.github.com/en/actions/reference/security/secure-use
- Dependabot options reference: https://docs.github.com/en/code-security/dependabot/working-with-dependabot/dependabot-options-reference
- LiteLLM security update, March 2026: https://docs.litellm.ai/blog/security-update-march-2026
- Go toolchains: https://go.dev/doc/toolchain
- govulncheck tutorial: https://go.dev/doc/tutorial/govulncheck
- testcontainers for Go: https://golang.testcontainers.org/
- First green CI run: https://github.com/StphnLwnga/llm-gateway/actions/runs/38146280817
- Code: `deploy/docker/Dockerfile`, `.golangci.yml`, `.github/workflows/ci.yml`, `.github/dependabot.yml`, `readme_test.go`
