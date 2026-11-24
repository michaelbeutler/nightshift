# Project rules

Conventions every contributor (human or agent) follows in this repo, enforced in review.
[CONTRIBUTING.md](CONTRIBUTING.md) says how to build, test and release; this file and
`.claude/rules/` say what a change is held to. Link to them rather than restating them.

## What this is

- `github.com/michaelbeutler/nightshift` is a CLI: "schedule send" for git. During the day
  `nightshift commit` buffers the index as a snapshot, `push` and `pr create` plan the push
  and the pull request; at night the runner (`nightshift run`, started every minute by the
  OS service) creates the real commits, dated at their slot and signed then, pushes them,
  and opens the pull or merge request.
- `internal/engine` is the product: buffering, planning, the runner, `doctor`. `internal/cli`
  (Cobra + Viper) and `internal/mcp` (the stdio MCP server) are thin layers over it and hold
  no logic of their own. `internal/git` runs the system git, `internal/provider` opens pull
  requests (GitHub, GitLab), `internal/queue` stores jobs, `internal/schedule` parses times,
  `internal/service` installs the runner, `internal/config` loads settings.
  [docs/how-it-works.md](docs/how-it-works.md) is the design.

## Use what is installed

- **Git is the system's git.** Every repository operation is a `git` invocation through
  `internal/git`; nightshift never parses `.git/` itself or links a git library. That is how
  SSH keys, agents, credential helpers, `insteadOf`, signing (`gpg.format`), hooks and
  `includeIf` all keep working without nightshift knowing about them.
- **Pull requests prefer the user's CLI.** `gh`/`glab` when installed and logged in, else the
  REST API with a token found at use time (`internal/provider/auth.go`). A provider
  difference is a field of `config.Host` or a method of the provider, never a branch on a
  host name outside `provider.detect`.
- Dependencies are Cobra and Viper, nothing else. A new module needs a reason in the PR;
  the MCP server is hand-written on purpose.

## Invariants

These hold for every change; a change that weakens one is refused, not discussed.

- **Scheduling never publishes.** `commit`, `adopt`, `push`, `pr create`, `reschedule` and
  `cancel` change local state only. Only the runner pushes, signs and opens pull requests.
- **Commit dates are the slot.** A commit's author and committer dates are its job's `At`,
  never the time the runner happens to run, so a run that catches up after sleep still
  dates the commit at night.
- **Published history is never rewritten.** `adopt` refuses commits reachable from any
  remote; the runner moves a branch only with a compare-and-swap `update-ref` from the
  commit it expects, and fails the job (it never forces) when the branch moved.
- **The working tree is the user's.** Buffering and canceling leave the index and files
  alone (beyond the `git add` the user asked for). The only write is the runner's
  `read-tree -m -u` fast-forward in `syncWorktree`, which refuses to overwrite local changes.
- **Unattended means bounded and silent.** Everything the runner starts has a timeout, no
  stdin, and `git.NonInteractive` in its environment; `doctor` tries push and signing
  detached from the terminal (`git.Exec.Detach`) so it fails the way the night would.
  Rules: [`.claude/rules/unattended.md`](.claude/rules/unattended.md).
- **No secret is stored, printed or logged.** Tokens are looked up when a pull request is
  opened and dropped; job files hold no credentials and are written `0600`. Errors from a
  server or a tool go through `provider.clean` (one line, no escape sequences, capped).
- **stdout belongs to the protocol in `nightshift mcp`.** Progress, hook output and logs go
  to stderr (`Engine.Log`).
- **Output for people and for machines.** Every command prints for people by default and
  JSON with `--json`; with `--json` an error is `{"error": ...}` on stdout and exit code 1.

## Commands

- `just check` is the pre-PR gate (fmt, lint, vet, race tests with coverage, govulncheck,
  tidy, build). `just test` runs the unit tests (`just test -run TestAdopt ./internal/engine`).
- `just demo` re-records `demo.gif` (`demo.tape`, `scripts/demo-setup.sh`; needs vhs, ttyd,
  ffmpeg). Re-record it when what the demo shows changes.
- `just install-hooks` once, for the pre-commit and commit-msg hooks.

## Commits and pull requests

- Conventional Commits for every subject and every PR title, checked by
  `scripts/check-commit-msg.sh` (the commit-msg hook and CI). Scopes are in CONTRIBUTING.md.
- Branch off `main`, one change per PR with its tests and docs, and link the issue in the
  PR body (`Closes #N`). A change to a command, a flag, a config key or what the runner does
  updates the README, `docs/` and the MCP tool descriptions in the same PR.

## Rule files

[`code-economy.md`](.claude/rules/code-economy.md) (search before you write, one owner per
rule, comments that say what is true now), [`go-testing.md`](.claude/rules/go-testing.md)
(real git in temp repos, a fake clock, no network),
[`unattended.md`](.claude/rules/unattended.md) (code that runs while nobody watches).
