# 0001. Record decisions as MADR ADRs, one per decision

- Status: Accepted
- Date: 2026-10-11
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 0

## Context

The gateway's design decisions build on each other: the data model depends on the storage choice, the deployment on the shutdown design, the pipeline on the broker. A decision that lives only in a pull request thread or a chat is hard to find later, and its rejected options are lost. The repo needs one place where each decision, the options it beat, and the reasons are written down and reviewed like code.

## Considered options

1. Nygard ADR: title, status, context, decision, consequences.
2. MADR (Markdown Architectural Decision Records): the Nygard sections plus considered options and pros and cons per option.
3. Y-statement: one sentence per decision ("In the context of ..., facing ..., we decided for ... to achieve ..., accepting ...").

## Pros and cons

| Option | Pros | Cons |
|---|---|---|
| Nygard | Short and widely known. Low cost to write. | No section for the rejected options, so a reader cannot see what the choice was compared with. |
| MADR | Records every option with its pros and cons, the outcome, and the consequences. Plain Markdown, reviewed in pull requests. A maintained template with versioned releases. | Longer to write than Nygard. Needs discipline to keep each record to one decision. |
| Y-statement | Fits in one sentence. Useful as a summary line. | Too short for a decision with several options or measured trade-offs. No room for links or consequences. |

## Decision outcome

Chosen: MADR, in a trimmed form, with one ADR per decision. Each record has a title, Status, Date, Decision-makers, Implemented in, Context, Considered options, a pros and cons table, Decision outcome, Consequences and Links. A decision is recorded when it is made, even when its code comes in a later milestone.

Files are `docs/adr/NNNN-kebab-title.md`, and `docs/adr/README.md` is the index. MADR's default folder is `docs/decisions/`; this repo uses `docs/adr/`.

## Consequences

- Good: every decision can be reviewed in the same pull request as the code it shapes. Later code and records can link the exact record they build on.
- Good: a decision that is accepted but not built yet says so ("Implemented in: Milestone 6 (planned)"), so the status of the code is never implied by the record.
- Bad: more files to keep current. A decision that changes needs a new ADR that supersedes the old one, not an edit in place.

## Links

- MADR: https://adr.github.io/madr/
- Michael Nygard, "Documenting Architecture Decisions": https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions
- ADR templates, including the Y-statement: https://adr.github.io/adr-templates/
