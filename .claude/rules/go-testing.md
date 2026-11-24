# Go testing conventions

Enforced in review (and, where noted, by `just lint`). Applies to every `*_test.go` file.

## Every test is parallel and self-contained

- `t.Parallel()` is the first statement of every top-level test. A test stays serial only
  when it touches process-wide state, and says why (the CLI tests in `internal/cli` set
  `NIGHTSHIFT_HOME` and git's config through `t.Setenv`).
- The context is `t.Context()`, files live under `t.TempDir()` (usetesting enforces both).
- **Real git, no network.** Engine tests run the installed git against repositories in a
  temp directory, with a bare repository as the remote. Pull request tests use an
  `httptest` server for the API and fake `gh`/`glab` through `provider.Env` (`Run`,
  `LookPath`, `Getenv`); nothing reaches github.com or gitlab.com.
- **The developer's git config never leaks in.** `TestMain` in `internal/engine` points
  `GIT_CONFIG_GLOBAL` at a file of its own and sets `GIT_CONFIG_NOSYSTEM`; a test that
  needs signing or a hook configures it in its repository.
- **No `time.Sleep` to wait for time to pass.** The engine's clock is a field
  (`Engine.Now`); the fixture's `runAt(t)` moves it and runs the queue, so a night passes
  in a millisecond.
- The suite runs with `-race` in `just check` and CI.

## Which helper

| You need | Use | Where |
| --- | --- | --- |
| A repository on branch `feature` with a bare remote, and an engine with a controlled clock | `newFixture(t)`; `f.write`, `f.git`, `f.remoteGit`, `f.commit`, `f.runAt`, `f.job`, `f.hook` | `internal/engine/helpers_test.go` |
| A GitHub or GitLab API | `httptest.NewServer` and a `config.Host{API: srv.URL}` | the provider or engine test |
| `gh`, `glab`, `ssh -G`, `git credential` answers | `fakeTools` | `internal/provider/provider_test.go` |
| The CLI end to end | `sandbox(t)` and `nightshift(t, dir, args...)` | `internal/cli/cli_test.go` |
| An MCP session | `serve(t, server, lines...)` | `internal/mcp/server_test.go` |

## Assertions: standard library only

- No testify or any other assertion module. Check with `if` and fail with `t.Fatalf`,
  naming the operation, what came back, and what was wanted.
- `t.Fatalf` when a later line would panic or mean nothing; `t.Errorf` for independent
  checks reported from one run.
- Errors are checked with `errors.Is` against the sentinel (`engine.ErrNothingToCommit`,
  `queue.ErrNotFound`) or `errors.As` (`*engine.BranchMovedError`), not by comparing
  messages, except where the message is the interface (CLI hints).

## Proportion

- One property, one tier: a rule is a table of cases in the file of the code that owns it
  (`schedule_test.go` for time parsing, `url_test.go` for remote URLs); a flow test asserts
  what the flow adds.
- The name states the property (`TestCatchUpKeepsNightDates`).
