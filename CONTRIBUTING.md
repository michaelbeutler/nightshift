# Contributing to nightshift

Thanks for contributing. nightshift is "schedule send" for git: you prepare
commits, the push and the pull request during the day, and a background
runner creates, signs, pushes and opens them in the night window. It does
that unattended, with the user's own git, keys and tokens, so we hold a high
bar for correctness, security and tests, while keeping the workflow light
enough for a small project.

The conventions here follow cobra-oauth2's and nauthera-server's, scaled to
a command-line application. If anything here conflicts with the code, the
code wins; open a PR to fix the doc.

---

## 1. Prerequisites & getting started

- **Go**, at least the version in `go.mod`'s `go` directive. CI tests that
  minimum and the latest stable release, and releases are built with the
  latest stable; develop on the latest stable.
- **git 2.36 or later** on `PATH`. nightshift runs the installed git rather
  than reimplementing it (2.36 brought `git hook run`), and so do its tests.
- **[just](https://github.com/casey/just)** for the task runner (optional: every
  recipe is a plain command you can copy out of the `justfile`).
- **[golangci-lint](https://golangci-lint.run/) v2**, at the version CI pins
  (`GOLANGCI_LINT_VERSION` in `.github/workflows/ci.yml`, `golangci_lint_version`
  in the `justfile`, currently v2.14.0). Install it with `just install-tools`,
  or with brew or the official install script, and check `golangci-lint version`.
  The version matters: a different one reports different findings, and a
  build made with an older Go than yours panics on the newer standard library.
  `just lint`, `just fmt` and the pre-commit hook check both
  (`scripts/check-golangci-lint.sh`) and print an install hint instead.
- **govulncheck** needs no install: `just vuln` runs the pinned version with
  `go run`.
- Only if you need them: **[goreleaser](https://goreleaser.com)** v2 for
  `just snapshot` (§ 8), and **[vhs](https://github.com/charmbracelet/vhs)**,
  **ttyd** and **ffmpeg** to re-record the demo with `just demo`.

`gh` and `glab` are not needed to develop: the tests replace them with fakes
(§ 5).

First run:

```bash
just install-hooks   # one-time: the pre-commit and commit-msg hooks (§ 2)
just test            # unit tests
just check           # the full local gate
```

---

## 2. Everyday commands

```bash
just                 # list the recipes
just check           # the full local gate: run this before every PR
just test            # go test ./...   (args pass through: just test -run TestPick ./internal/schedule)
just test-race       # race detector
just cover           # coverage profile + coverage.html
just lint            # golangci-lint run (.golangci.yml)
just fmt             # format (gofumpt + goimports through golangci-lint)
just vet             # go vet, for your platform and for Windows
just tidy            # go mod tidy + verify
just vuln            # govulncheck, pinned
just build           # bin/nightshift
just install         # go install . into $(go env GOPATH)/bin
just run doctor      # go run . doctor (args pass through)
just snapshot        # every release archive into dist/, nothing published (§ 8)
just release-notes v0.2.0   # preview a release's notes (§ 8)
```

`just run` is your real nightshift: your config, your state directory, your
queue, your credentials. Try things in a throwaway repository with a
throwaway remote, not in the project you are working on.

### Two tiers: the hooks and `just check`

`just check` runs formatting, lint, vet, race tests with coverage, govulncheck,
the tidy check and a build: what CI runs, on your platform. Run it **before
opening a PR**, not on every commit.

For the per-commit tier, `just install-hooks` writes two plain git hooks (no
hook framework, the same mechanism as cobra-oauth2 and nauthera-server):

- **pre-commit** checks that the **staged** content of each `.go` file is
  formatted, and runs `go vet` on the packages they belong to. It never
  rewrites or re-stages anything, so hunks you left out with `git add -p`
  stay out of the commit. When a file is not formatted it stops and asks you
  to run `just fmt` and stage the result. It takes a second or two.
- **commit-msg** checks the message against Conventional Commits (§ 8) with
  `scripts/check-commit-msg.sh`, the script CI runs on the pull request.

Bypass both for a single commit with `git commit --no-verify`, and re-run
`just install-hooks` after the recipe changes.

---

## 3. Repository layout

- `main.go` hands the signals and the standard streams to `cli.Execute`;
  nothing else happens there.
- `internal/cli`: the command line (Cobra and Viper): commands, flags, human
  and `--json` output, and the tools `nightshift mcp` offers.
- `internal/engine`: nightshift itself. Buffers commits, plans each branch's
  commits, push and pull request into the night window, runs them when the
  runner calls, and `doctor`. The CLI and the MCP server are thin layers
  over it.
- `internal/git`: runs the installed git (commits, signing, hooks, pushes)
  and parses remote URLs. nightshift never reimplements git.
- `internal/queue`: the job store, one JSON file per job in the state
  directory, with a lock so one runner works at a time.
- `internal/config`: settings from defaults, the config file, `NIGHTSHIFT_*`
  variables and flags; the template `nightshift config init` writes.
- `internal/provider`: opens pull requests (GitHub) and merge requests
  (GitLab) through `gh`/`glab` or the REST API, finds the token, and
  detects which host a remote belongs to.
- `internal/mcp`: a small Model Context Protocol server over stdio, so
  agents can queue work.
- `internal/service`: installs the runner with the OS's own scheduler
  (launchd, a systemd user timer, a Windows scheduled task).
- `internal/notify`: best-effort desktop notifications.
- `internal/schedule`: turns "tonight", "02:30" or "+2h" into an instant and
  picks natural-looking times inside the night window.
- `internal/version`: which build is running (`nightshift version`).
- `scripts/` holds the shell scripts the hooks, CI and releases share.

---

## 4. Code style & conventions

- **`gofumpt`/`goimports` clean.** Run `just fmt`; the pre-commit hook checks it.
- **Match the surrounding code.** Comment density, naming and idiom should be
  indistinguishable from the file you're editing.
- **Doc comments on every exported identifier** (revive enforces it, even
  though every package is internal). Explain *why* and what the caller can
  rely on, not the signature.
- **Errors are wrapped with `%w`** and say what was being done, lowercase and
  without trailing punctuation: `push main to origin: %w`, never "failed to
  …", which a wrapped chain repeats at every level. Sentinels are `ErrXxx`,
  error types `XxxError` (errname enforces it). An error the runner records
  on a job is what the user reads the next morning: make it say what to do.
- **Context everywhere.** Every subprocess is started with
  `exec.CommandContext` and every HTTP request with
  `http.NewRequestWithContext` (noctx enforces it), so Ctrl-C, a service
  stop or a timeout stops them.
- **Injectable outside world.** The clock (`Engine.Now`), randomness
  (`Engine.Rand`), the network, `PATH` and the environment
  (`provider.Env`), notifications and the output writers are fields with
  real defaults, so tests replace them.
- **No printing.** Write to the writer you were given (the cobra command's
  `OutOrStdout()`/`ErrOrStderr()`, `Engine.Log`). `fmt.Print*` and the
  global `log` are refused by forbidigo. Under `nightshift mcp`, stdout is
  the protocol, and a stray print breaks the client.
- **Subprocesses are deliberate.** gosec runs in audit mode, so starting a
  program with a variable name or arguments is a finding. Where it is the
  point (git, gh, glab, the editor, a `token_cmd`), say why on the line:
  `//nolint:gosec // G204: <why>`.
- **Minimal dependencies.** Prefer the standard library: go.mod has Cobra and
  Viper, and the MCP server is hand-written to keep it that way. A new
  dependency needs a justification in the PR.

---

## 5. Testing

- **Real git, throwaway repositories.** Tests that touch git create
  repositories (and bare "remotes") in `t.TempDir()` and run the real git
  in them. That is the point: nightshift's behaviour is git's behaviour.
  Never touch the developer's own repositories, config or state directory;
  point the engine's store and config at the temp directory too.
- **A fake clock.** The engine's tests set `Engine.Now` to a clock the test
  advances, so "is this job due yet?" is decided by the test, not by the
  time of day CI happens to run. No `time.Sleep` to wait for a schedule.
- **No network.** Provider tests answer the REST API from `httptest`
  servers, and replace `gh` and `glab` with fakes through the injectable
  `provider.Env` (its `Run`, `LookPath` and `Getenv`), which record the
  commands and answer from a table. No test needs a GitHub or GitLab
  account, a token or `gh`/`glab` installed.
- **Table-driven tests** for pure logic (schedule parsing, URL parsing, host
  detection), one behaviour per case with a clear name. Use `t.Parallel()`
  where the test owns its state.
- **Race detector** (`just test-race`) for anything with goroutines, the
  queue lock or shared state; `just check` and CI always run with `-race`.
- **Every platform.** CI runs the tests on Linux, macOS and Windows. Use
  `filepath`, not string paths; build-tag platform code into
  `_unix.go`/`_windows.go` pairs, as `internal/git` and `internal/queue` do.
- **Security invariants get explicit tests**: a token never reaches a job
  file, an error or the output; a job pushes exactly the remote and ref it
  names; a force push only happens when asked for.
- **Coverage.** New code should not lower the coverage of the package it lives
  in. `just cover` writes `coverage.html`; CI prints the total in the job summary.

---

## 6. Security practices

nightshift acts for the user while they sleep. Rules for code in this
repository (SECURITY.md says what a vulnerability report is measured
against):

- **Never store a token.** Read it when it is needed (`token_cmd`, the
  environment, `gh`/`glab`, the git credential helper) and keep it in memory
  for that request only. It never reaches a job file, the log, an error
  message or the output.
- **Do what was queued, nothing more.** The runner pushes the remote and ref
  the job names, commits the tree that was snapshotted, and opens the pull
  request that was described. Anything that changes what runs at night says
  so in the PR.
- **Repository content is data.** A file, branch name, commit message,
  remote URL, PR template or API response must never become a command or a
  command's argument in a way that changes what runs. Pass arguments as
  arguments, never through a shell, and end option parsing (`--`) before
  anything user-supplied.
- **Private state.** The state directory is `0700` and job files `0600`.
- **Dependencies are part of the attack surface.** `govulncheck` runs on every
  PR; keep the dependency list short.
- If your change has a security dimension you are unsure about, say so in the
  PR's "What runs unattended" section and ask for review. To report a
  vulnerability, follow [SECURITY.md](SECURITY.md), never a public issue.

---

## 7. Good first contributions

Welcome! These are self-contained, wanted, and small enough to finish in a
few evenings. Comment on (or open) an issue to say you are on one, so two
people do not build the same thing, and ask as many questions as you like;
a draft PR early is better than a perfect one late.

- **A Gitea / Forgejo provider** (Codeberg and self-hosted instances).
- **A Bitbucket provider** (Cloud, and Data Center if you have one to test
  against).
- **An Azure DevOps provider.**

  Each provider is the same shape: implement `provider.Provider` in a new
  file in `internal/provider` (GitHub and GitLab are the examples to follow,
  including "the pull request already exists" and a CLI-or-API choice if the
  host has a CLI), teach `detect` to recognise the host, and give `Check`
  a clear answer for `nightshift doctor`. Test it against an `httptest`
  server through `provider.Env`, the way `provider_test.go` does: no real
  account needed. The **Provider request** issue template collects what you
  need to know about the host's API.
- **GitLab merge requests from forks.** Today `nightshift pr create` into
  another project returns "not supported yet" (`internal/provider/gitlab.go`);
  GitHub already supports `target_repo`. The API needs the source and target
  project ids.
- **Windows notifications.** `internal/notify` covers macOS (osascript) and
  Linux (notify-send); Windows has none. Keep it best-effort and free of new
  dependencies (a PowerShell toast is one option).
- **Packaging: a Homebrew tap, a winget manifest, a Scoop manifest.**
  `.goreleaser.yaml` has a commented-out Homebrew block that says what to set
  up; goreleaser can write winget and Scoop manifests too.
- **`nightshift log`** to show and follow the runner's log file, so nobody
  has to look up where it lives first (`nightshift config path` prints the
  path today).
- **Shell completion docs.** Cobra already generates completions
  (`nightshift completion bash|zsh|fish|powershell`); document how to install
  them for each shell, and check that dynamic completion of job ids would be
  worth adding.

---

## 8. Commits & pull requests

- **Branch** off `main`; do not commit to it directly.
- Keep PRs focused: one change, its tests and its docs.
- Never commit build output (`bin/`, `dist/`, `./nightshift`), coverage
  files, `.env` files or real tokens.

### Conventional Commits

Every commit subject and every PR title follows
[Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):

```
type(scope)!: description

optional body, wrapped at 72 columns, saying why

BREAKING CHANGE: what a user must change (optional footer)
```

**Types** (the ones `scripts/check-commit-msg.sh` accepts and
`scripts/release-notes.sh` groups by):

| type       | for                                                   | in the release notes |
| ---------- | ----------------------------------------------------- | -------------------- |
| `feat`     | a new capability for users                            | Features             |
| `fix`      | a bug fix                                             | Bug fixes            |
| `perf`     | a performance improvement                             | Performance          |
| `revert`   | reverting an earlier change                           | Reverts              |
| `docs`     | documentation only                                    | Documentation        |
| `refactor` | a change that neither fixes a bug nor adds a feature  | Maintenance          |
| `test`     | tests only                                            | Maintenance          |
| `build`    | build system, `go.mod`, goreleaser, tooling           | Maintenance          |
| `ci`       | workflows and the scripts CI runs                     | Maintenance          |
| `chore`    | anything else that does not change nightshift itself  | Maintenance          |
| `style`    | formatting only                                       | Maintenance          |

**Scopes** are optional, lowercase, and name the area a reader of the release
notes would look for. The usual ones: `cli`, `engine`, `git`, `queue`,
`config`, `provider`, `github`, `gitlab`, `mcp`, `service`, `schedule`,
`docs`, `deps`, `ci`. Use a new one when none fits (a new provider gets its
own, `gitea`, say); several are joined with a comma (`fix(engine,git):`).
`deps` is special: the release notes fold it into the "Dependencies" block,
and Dependabot uses it (`chore(deps):`, `ci(deps):`).

**Descriptions** are imperative and lowercase-first unless they start with a
name (`feat(gitlab): open merge requests from forks`), with no trailing
period. Keep the subject to 72 characters; the check refuses more than 100.

**Breaking changes** (anything that makes a user change their config, a
command or a script reading `--json`, or that changes what happens at night
with an unchanged config) get a `!` after the type or scope **and** a
`BREAKING CHANGE:` footer explaining the migration:

```
feat(config)!: rename schedule.window to schedule.night

BREAKING CHANGE: the night window is now set with schedule.night. Rename
the key in your config file; the value keeps its format.
```

The release notes list either form under "Breaking changes" first, and that
footer is what users read before upgrading, so say there what to change.
Before 1.0, a breaking change bumps the minor version.

`git commit --fixup` / `--squash` commits are accepted locally by the hook,
but CI refuses them in a PR: squash them before merging
(`git rebase -i --autosquash main`). Subjects git writes itself (`Merge …`,
`Revert "…"`) are accepted.

### Pull requests

- The **PR title** is a Conventional Commit too, and it matters most: it
  becomes the merge commit's subject (the repository's merge setting uses the
  PR title, for merge and squash merges alike) and so the line in the release
  notes. CI checks the current title, so after fixing it, re-run the job.
- Fill in the template: what and why, the type of change, breaking changes,
  **what runs unattended** ("No" is a valid answer, but say so), and how you
  tested it.

### What CI runs

`.github/workflows/ci.yml` runs on every pull request and every push to
`main`; a newer push to a PR cancels the older run.

| job                                | what                                                                    | locally                |
| ---------------------------------- | ----------------------------------------------------------------------- | ---------------------- |
| actions are pinned by commit SHA   | every `uses:` is a full commit SHA with a `# vX.Y.Z` comment            | (read the diff)        |
| lint                               | golangci-lint (formatting included), `go vet`                           | `just lint`, `just vet` |
| test                               | race tests with coverage on Linux, macOS and Windows, on go.mod's minimum Go and the latest stable, against the runner's git; the coverage total in the job summary of one of them | `just test-race-cover` |
| known vulnerabilities              | govulncheck on the latest stable Go                                     | `just vuln`            |
| go.mod and go.sum are tidy         | `go mod tidy -diff`, `go mod verify`                                    | `just tidy-check`      |
| build every platform               | `go build` for linux, darwin and windows on amd64 and arm64, and `goreleaser check` | `just build`, `just vet`, `just snapshot` |
| conventional commits (PRs only)    | the PR title and every commit subject                                   | the commit-msg hook    |

`just check` covers everything but the other platforms, the minimum Go, the
SHA pins, `goreleaser check` and the PR title.

Every action is pinned by commit SHA, and each job has the least permissions
it needs (read-only unless it says otherwise). Dependabot
(`.github/dependabot.yml`) proposes Go module and action updates weekly,
grouped, after a short cooldown, and security updates right away. Renovate
is switched off (`.github/renovate.json`).

### Releasing

A pushed tag is the only thing that publishes, through
`.github/workflows/release.yml`, which runs goreleaser (`.goreleaser.yaml`):
binaries for macOS, Linux and Windows on amd64 and arm64, as `.tar.gz` (and
`.zip` for Windows) with the README and the LICENSE, a `checksums.txt`, and
a GitHub release. `go install github.com/michaelbeutler/nightshift@vX.Y.Z`
works from the tag alone.

- `vX.Y.Z` is a release. It must name a commit that was `main`'s head.
- `vX.Y.Z-alpha.N`, `vX.Y.Z-beta.N` and `vX.Y.Z-rc.N` are pre-releases,
  published as GitHub pre-releases, and may be tagged on any branch.

Pick the version from what landed since the last release, by semantic
versioning: before 1.0, a breaking change bumps the minor version and
everything else the patch version; from 1.0 on, a feature (`feat`) bumps
the minor version, a fix or a dependency update the patch version, and a
breaking change the major version. Then:

```bash
git switch main && git pull
just snapshot                 # optional: build every archive locally, publish nothing
git tag -a v0.2.0 -m v0.2.0
just release-notes v0.2.0     # preview; `git tag -d v0.2.0` backs out before the push
git push origin v0.2.0
```

The workflow checks the tag, runs the build and tests once more, writes the
notes with `scripts/release-notes.sh` (the conventional subjects since the
previous `vX.Y.Z`, grouped by type and sorted by scope, with maintenance and
dependency updates folded away), and hands them to goreleaser, which builds
with the latest stable Go and publishes the release. A release with a
serious bug is not deleted: fix forward, and say in the next release's
notes what was wrong.

### Definition of done (checklist before you open a PR)

- [ ] **`just check` passes** (fmt, lint, vet, race tests, govulncheck, tidy,
      build).
- [ ] New or changed behaviour has tests, with real git in `t.TempDir()` and
      no network; a security invariant it touches has its own test.
- [ ] The README and the docs match the commands, flags and config keys.
- [ ] No token reaches a job file, the log, an error message or the output.
- [ ] The PR says whether it changes what runs unattended.
- [ ] The PR title and the commits follow Conventional Commits; a breaking
      change has `!` and a `BREAKING CHANGE:` footer saying what to change.
- [ ] A new dependency is justified in the PR description.

---

Questions, or a design you want to sanity-check before building it? Open a
draft PR or an issue. Thank you for keeping the bar high.
