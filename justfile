# justfile — task runner for nightshift.
# Install just: https://github.com/casey/just  |  Run `just` to list targets.
#
# Every recipe is a plain command you can copy out of this file; just is a
# convenience, not a requirement.

set positional-arguments

# ---- Pinned tool versions ---------------------------------------------------
# Keep these equal to .github/workflows/ci.yml (GOLANGCI_LINT_VERSION,
# GOVULNCHECK_VERSION) so local and CI results agree, and close to
# cobra-oauth2's and nauthera-server's pins so the projects lint alike. Bump
# them together.
#
# govulncheck runs with `go run pkg@version` rather than a go.mod `tool`
# directive: that is just as reproducible, and keeps the tool's dependency
# graph out of the go.mod that `go install` users resolve.
golangci_lint_version := "v2.14.0"
govulncheck_version   := "v1.10.0"

# Coverage profile written by `test-race-cover` and `cover`.
coverprofile := "coverage.out"

# Default target: show the available recipes.
default:
    @just --list

# ---- The two gates ------------------------------------------------------------

# `just check` is the pre-PR gate. It runs what CI's lint, test, vuln and tidy
# jobs run, plus a build, on your platform and Go only (CI adds macOS, Windows,
# the oldest Go that go.mod allows, a build for every GOOS and `goreleaser
# check`). It does not check commit subjects or the pull request title: the
# commit-msg hook (`just install-hooks`) covers the first, and the second only
# exists on GitHub.
#
# Run the full local gate before opening a pull request.
check: fmt-check lint vet test-race-cover vuln tidy-check build
    @echo "==> all checks passed"

# pre-commit is the cheap tier: it only checks what is STAGED. Everything
# expensive (race tests, coverage, govulncheck) stays in `just check` and CI.
# commit-msg holds each message to Conventional Commits with
# scripts/check-commit-msg.sh, the same script CI runs on a pull request's
# title and commits.
#
# The pre-commit hook never rewrites or re-stages files. Re-staging a whole
# file after formatting it would also commit the hunks left out with
# `git add -p`. It checks the staged content instead (what `git show :file`
# prints) and, when that is not formatted, stops with a hint to run
# `just fmt` and stage the result.
#
# Deliberately plain git hooks, as in cobra-oauth2 and nauthera-server, not
# the `pre-commit` framework or lefthook: two short scripts do not need
# another toolchain. Re-run this recipe after editing the hook bodies. Bypass
# a single commit with `git commit --no-verify`.
#
# Install the git hooks: a fast pre-commit hook (~1-2s) and a commit-msg hook.
install-hooks:
    #!/usr/bin/env bash
    set -euo pipefail
    hooks="$(git rev-parse --git-path hooks)"
    mkdir -p "$hooks"

    cat >"$hooks/pre-commit" <<'HOOK'
    #!/usr/bin/env bash
    # Managed by `just install-hooks` — re-run that recipe to update.
    # Fast tier only: check the formatting of the STAGED content and vet the
    # touched packages. Nothing is rewritten or re-staged. The full gate is
    # `just check`.
    set -euo pipefail
    root="$(git rev-parse --show-toplevel)"
    cd "$root"

    # NUL-separated, so paths with spaces survive. --diff-filter=d skips
    # deletions (nothing left to check).
    staged=()
    while IFS= read -r -d '' f; do
        staged+=("$f")
    done < <(git diff --cached --name-only -z --diff-filter=d -- '*.go')
    [ "${#staged[@]}" -gt 0 ] || exit 0

    if [ -x scripts/check-golangci-lint.sh ]; then
        scripts/check-golangci-lint.sh || { echo "pre-commit: (or commit with --no-verify)" >&2; exit 1; }
    fi

    echo "pre-commit: golangci-lint fmt (staged content)"
    unformatted=()
    for f in "${staged[@]}"; do
        # The index version, not the working tree: unstaged hunks are not
        # part of this commit and must not decide the check.
        if ! git show ":$f" | golangci-lint fmt --stdin 2>/dev/null | cmp -s - <(git show ":$f"); then
            unformatted+=("$f")
        fi
    done
    if [ "${#unformatted[@]}" -gt 0 ]; then
        echo "pre-commit: not formatted (staged version):" >&2
        printf '    %s\n' "${unformatted[@]}" >&2
        echo "pre-commit: run 'just fmt', review the change and stage it" >&2
        exit 1
    fi

    # go vet is per-PACKAGE, not per-file: derive the touched packages
    # (main.go at the root becomes ./.).
    pkgs=()
    while IFS= read -r d; do
        pkgs+=("./$d")
    done < <(for f in "${staged[@]}"; do dirname -- "$f"; done | sort -u)
    echo "pre-commit: go vet ${pkgs[*]}"
    go vet "${pkgs[@]}"
    HOOK

    cat >"$hooks/commit-msg" <<'HOOK'
    #!/usr/bin/env bash
    # Managed by `just install-hooks` — re-run that recipe to update.
    # Conventional Commits, checked by the script CI runs (CONTRIBUTING.md).
    set -euo pipefail
    script="$(git rev-parse --show-toplevel)/scripts/check-commit-msg.sh"
    # A branch from before the script existed has nothing to run.
    [ -x "$script" ] || exit 0
    exec "$script" "$1"
    HOOK

    chmod +x "$hooks/pre-commit" "$hooks/commit-msg"
    echo "==> installed $hooks/pre-commit and $hooks/commit-msg"
    echo "    pre-commit: formatting check + go vet (staged .go, nothing rewritten)"
    echo "    commit-msg: Conventional Commits (scripts/check-commit-msg.sh)"
    echo "    bypass once with: git commit --no-verify"

# ---- Everyday -----------------------------------------------------------------

# Extra args go to `go test` and replace ./...: `just test -run TestPick ./internal/schedule`.
# The tests shell out to a real git (see CONTRIBUTING.md, "Testing").
#
# Run the unit tests.
test *args:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ $# -eq 0 ]; then set -- ./...; fi
    go test "$@"

# Run the unit tests with the race detector (needs cgo).
test-race:
    CGO_ENABLED=1 go test -race ./...

# -coverpkg counts a statement reached from another package's test too: the
# engine's tests drive the git and queue packages, and that counts for them.
#
# Race tests and the coverage profile in one pass, as CI's test job runs them.
test-race-cover:
    CGO_ENABLED=1 go test -race -covermode=atomic -coverpkg=./... -coverprofile={{coverprofile}} ./...

# Prints the total; `go tool cover -func=coverage.out` prints the
# per-function table afterwards.
#
# Write the coverage profile and an HTML report (coverage.html) to open in a browser.
cover: test-race-cover
    go tool cover -func={{coverprofile}} | tail -n 1
    go tool cover -html={{coverprofile}} -o coverage.html
    @echo "==> wrote coverage.html"

# A plain build: no -X flags, so `nightshift version` reports the VCS commit
# the Go toolchain stamps into the binary. Release builds (.goreleaser.yaml)
# set the version, commit and date through -ldflags instead.
#
# Build the binary into bin/nightshift.
build:
    go build -o bin/nightshift .

# Install nightshift into $(go env GOPATH)/bin, as `go install` users get it.
install:
    go install .

# Args go to nightshift: `just run doctor`, `just run commit -m "feat: x"`.
# This is your real nightshift (your config, your state dir, your queue), not
# a sandbox: point it at a throwaway repository when trying things out.
#
# Run nightshift from source.
run *args:
    go run . "$@"

# vhs: https://github.com/charmbracelet/vhs. Needs vhs, ttyd and ffmpeg on
# PATH; nothing else in the repository does.
#
# Re-record the demo from demo.tape with vhs.
demo:
    vhs demo.tape

# Lint with golangci-lint (uses .golangci.yml; formatters included).
lint: golangci-lint-version
    golangci-lint run

# Format the codebase (gofumpt + goimports via golangci-lint v2 formatters).
fmt: golangci-lint-version
    golangci-lint fmt

# Fail if anything is not formatted, without rewriting it.
fmt-check: golangci-lint-version
    golangci-lint fmt --diff

# A golangci-lint other than the pinned one, or one built with an older Go
# than yours, fails here with an install hint instead of a stack trace.
#
# Check that golangci-lint on PATH is the pinned version (scripts/check-golangci-lint.sh).
golangci-lint-version:
    @scripts/check-golangci-lint.sh

# The second pass type-checks the other side of the platform build tags
# (internal/git/detach_*.go, internal/queue/lock_*.go), which neither the
# first pass nor golangci-lint sees on your machine. CI builds every GOOS too.
#
# go vet every package, for this platform and for Windows.
vet:
    go vet ./...
    GOOS=windows go vet ./...

# Tidy and verify the module.
tidy:
    go mod tidy
    go mod verify

# Fail if go.mod or go.sum is not tidy, without rewriting them (CI's tidy job).
tidy-check:
    go mod tidy -diff
    go mod verify

# Scan for known vulnerabilities reachable from this module (govulncheck, pinned above).
vuln:
    go run golang.org/x/vuln/cmd/govulncheck@{{govulncheck_version}} ./...

# golangci-lint recommends its release binaries (brew, the install script) over
# `go install`; either is fine as long as `golangci-lint version` prints the
# pinned version. govulncheck needs no install: `just vuln` runs it pinned.
# goreleaser is only needed for `just snapshot` (brew install goreleaser; CI
# pins GORELEASER_VERSION in .github/workflows/release.yml).
#
# Install the pinned golangci-lint into $(go env GOPATH)/bin.
install-tools:
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@{{golangci_lint_version}}
    @echo "==> installed golangci-lint {{golangci_lint_version}}; make sure $(go env GOPATH)/bin is on PATH"

# ---- Releasing ----------------------------------------------------------------

# Tag locally first to preview, and delete the tag again if you are not
# releasing yet: `git tag -a v0.2.0 -m v0.2.0 && just release-notes v0.2.0`,
# then `git push origin v0.2.0` or `git tag -d v0.2.0`.
#
# Print the release notes a tag would get (scripts/release-notes.sh).
release-notes tag:
    scripts/release-notes.sh "$1"

# Builds every platform's archive into dist/ exactly as a release would, but
# publishes nothing and needs no tag (the version is a snapshot of HEAD).
#
# Build the release archives locally with goreleaser (nothing is published).
snapshot:
    goreleaser release --snapshot --clean
