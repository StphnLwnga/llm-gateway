## Summary

A session can now check a pull request body against the writing rules before it posts the body. Before, the rules were not in the agent instructions, and bodies drifted from plain English. The check finds the mechanical errors. A person must still read for plain words and one idea in each sentence.

- **The check reads a body from a file.** It gives each problem with its line number. It stops with an error when it finds a problem.
- **It finds the errors that drift caused before.** These are em dashes, en dashes, milestone numbers, decision numbers, contractions, the tool footer and session links.
- **It finds identifiers in the text.** These are file names, folder paths, package paths, and names in camel case, with underscores or with a package prefix. Product names such as OpenAI and macOS are permitted.
- **It permits one command.** A second code span or a second code block is a problem. A block between backticks, a block between tildes and an indented block all count.
- **It finds sentences that are longer than 25 words.** A sentence that wraps over lines or quote lines counts as one sentence. The line with the result of the checks is not counted.
- **The agent instructions have a new section for pull requests.** It gives the rules and tells sessions to run the check before they open or edit a pull request.
- **The pull request template has a new checklist line for the check.** The lines of the template are permitted, and text after them is still checked.

To check a body, run `go run ./tools/prbody <file>` from the repo root.

## Test plan

- [x] This body passes, and the empty template passes.
- [x] A body with drift gives exactly its 17 problems, line by line.
- [x] A template line passes, and a file path after it is a problem.
- [x] A second code block is a problem, with backticks, tildes or an indent. An indented line in a list item is not a code block.
- [x] A code block that is not closed is a problem, because it hides the text after it.
- [x] A sentence over three lines counts as one. Two quote lines count as one sentence, and the quote marks are not words.
- [x] The tool exits with 1 when it finds a problem and with 2 when it has no file.
- [x] The old bodies of two earlier pull requests give 4 and 17 problems.
- [x] The checks before every commit pass on Go 1.26.10: 183 tests and 224 subtests in 18 packages, 0 lint issues, 0 vulnerabilities.

## Docs

- [x] README or docs/ updated in this PR for any change to routes, settings, packages, behavior or measurements. The README layout lists the new tool, and the agent instructions have the new section.
- [ ] No docs change needed, because:

## Checklist

- [x] One purpose per commit (formatting changes in their own commit)
- [x] Comments say why, only where the code cannot (AGENTS.md)
- [ ] CI is green
- [x] `go run ./tools/prbody` passes on this body, and it reads as plain ASD-STE100 with no identifiers in the prose
