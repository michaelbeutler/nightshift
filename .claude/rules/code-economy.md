# Code economy

Enforced in review. Applies to every change, by a person or an agent.

Agent-written code fails in known ways: logic solved twice because the first copy was not
found, comments that narrate the editing session, abstractions with one caller, and tests
that outweigh the code. The yardstick is **lines a maintainer must read**, not lines
written. nightshift is small; keep it that way.

## 1. Search before you write

A helper you are about to write probably exists. Search by behaviour, not by the name you
had in mind (`grep -rn "UpdateRef" internal`, `grep -rn "func clean" internal`), and look
at the owner first:

| Rule | Owner |
| --- | --- |
| Running git, its errors, identities, dates, commit objects, refs | `internal/git/git.go` (`Repo.Do`, `Error`, `ParseIdent`, `FormatDate`, `CommitTree`, `UpdateRef`, `ReadCommit`) |
| Remote URLs in every form git accepts | `internal/git/url.go` (`ParseRemoteURL`) |
| What a typed time means, the window, spacing | `internal/schedule` (`Parse`, `ParseInstant`, `Window`, `Spacing`) |
| When a new job runs, keeping a branch's jobs in order | `internal/engine/engine.go` (`slot`, `reflow`, `dependents`, `plan`, `chain`) |
| What the runner does per kind, retries, catch-up | `internal/engine/run.go` (`claim`, `finish`, `runCommit`, `runPush`, `runPR`) |
| Which provider serves a host, CLI or API | `internal/provider/provider.go` (`Detect`, `detect`, `pickCLI`) |
| Finding a token | `internal/provider/auth.go` (`findToken`) |
| Making server or tool text safe to print | `internal/provider/provider.go` (`clean`, `cliError`), `api.go` (`apiMessage`) |
| Job storage and locks | `internal/queue` (`Store.Save`, `Find`, `Lock`, `TryLockRunner`) |
| Times and job lines for people | `internal/cli/ui.go` (`when`, `relative`, `table`, `details`, `meta`) |

The standard library comes before a helper of ours, and a helper of ours before a new
dependency (`slices`, `maps`, `min`/`max`, `strings.Cut*`). If what exists is almost
right, extend it; a second copy "with one difference" is the bug.

## 2. One owner per rule

A rule (a validation, a mapping, a default) lives in one place and every caller goes
through it. The CLI and the MCP server both call the engine; neither re-implements a
check the engine makes. Defaults live in `config.SetDefaults` and `template.yaml` only.

## 3. Abstractions pay rent

- No new package without a second importer; a function with one caller lives beside it.
- An interface is declared where it is consumed, with the methods that consumer uses, and
  only when there are two real implementations (`provider.Provider`). A seam for tests is a
  field (`Engine.Now`, `Engine.Env`, `provider.Env`), not an interface.
- No defensive branch for a state an earlier check rules out, no wrapper that forwards and
  adds nothing.

## 4. Comments say what is true now, and why

- **Keep**: the reason for a non-obvious choice, an invariant a caller must hold, a trap.
- **Delete**: a comment that restates the line below it, or a doc comment that repeats the
  signature.
- **Never write history**: no issue or PR numbers, no "used to", "previously", "moved
  here". Git holds the history.
- **A comment is true or it is deleted.** When you change what code does, the comment above
  it is part of the change, and so are the docs that describe it.

## 5. Tests are proportionate

One property, one tier, one fixture: a rule is pinned once, as a table in the test file of
the code that owns it, and a flow test asserts only what the flow adds. Build on the
helpers in `go-testing.md`.

## 6. Documentation is code

`README.md`, `CONTRIBUTING.md`, `docs/*.md`, `CLAUDE.md`, `.claude/rules/*.md`, the
command help and the MCP tool descriptions describe the tree as it is; a sentence that
became false is fixed in the PR that made it false. The README orients, CONTRIBUTING says
how to work, `docs/` explains the design, `.claude/rules/` holds what review enforces.
