# Security policy

nightshift runs while nobody is watching. At night it creates and signs
commits, runs `git push`, opens pull requests through `gh`, `glab` or a
host's REST API, and runs the token commands you configured, all with your
credentials and your signing key. A flaw here can leak a token, push to a
place you did not intend, or run something you never reviewed, so we treat
security reports as the highest-priority work on the project.

## Supported versions

nightshift is an application, not a library: fixes land on `main` and ship
in the next release, and only the **latest release** is supported. Upgrading
is the fix.

| Version                    | Supported          |
| -------------------------- | ------------------ |
| latest release             | :white_check_mark: |
| older releases             | :x: (upgrade)      |
| release candidates, `main` | best effort        |

## Reporting a vulnerability

Please report vulnerabilities **privately**, through GitHub's private
vulnerability reporting:

1. Open <https://github.com/michaelbeutler/nightshift/security/advisories/new>
   (repository → **Security** → **Report a vulnerability**).
2. Describe the issue, its impact, the affected version (`nightshift
   version`) or commit, and how to reproduce it (a failing test, or a script
   that sets up a throwaway repository, is ideal).

- **Do not** open a public issue, discussion or pull request for a
  vulnerability.
- **Do not** include live tokens or credentials for real accounts. Redact
  them, or reproduce against a throwaway repository and a token you revoke
  afterwards.

We will acknowledge the report, keep you updated while we work on a fix, agree
a disclosure date with you, and publish a GitHub security advisory (with a CVE
where one applies) when the fixed release is out. We are glad to credit
reporters who want to be named.

## How nightshift handles credentials

What a report is measured against:

- **nightshift never stores a token.** It asks for one when it needs it, at
  the moment it opens a pull request, from the host's `token_cmd`, the
  host's `token_env` or the provider's usual environment variables, `gh` or
  `glab`'s own login, or the git credential helper, in that order. The value
  lives in memory for that request and is not written to the job, the log,
  the config or an error message.
- **Pushing and signing are git's.** nightshift runs your git with your SSH
  keys, credential helpers, signing configuration and hooks; it does not
  handle keys itself.
- **Jobs are private files.** The queue lives in the state directory, in a
  `0700` directory of `0600` job files. A job records what to do (the
  repository, the commit message and tree, the remote and ref, the pull
  request fields), never a credential.
- **The runner acts on what you queued**, at the time you scheduled it, in
  the repository you queued it in.

## Scope

In scope: anything in nightshift that breaks those promises, for example

- a token, or the output of a `token_cmd`, that is printed, logged, written to
  a job file, the log file or the config, or put into an error message,
  a pull request body or a commit;
- a push to a remote, branch or ref other than the one the job names, a
  force push the job did not ask for, or a commit that differs from the one
  that was queued (content, author, date or signature);
- a command run because of something in **repository content** or remote
  data rather than your own configuration: a crafted file, branch name,
  commit message, remote URL, pull request template or API response that
  makes nightshift run a program, change its arguments, or read or write
  files outside the repository and its state directory;
- job files, the state directory or the log readable by other users, or a
  job file another user could plant for your runner to execute;
- the MCP server (`nightshift mcp`) letting a client do more than the tools
  it exposes (queue, inspect and change planned work), such as running a
  command of its choosing or reading files outside the repository it names;
- the installed service (`nightshift service install`) running something
  other than your nightshift binary, or as someone other than you.

Out of scope here, but welcome at the right place:

- vulnerabilities in git, `gh`, `glab`, a credential helper or a git host
  (report them to their maintainers);
- the commands you configure yourself (`token_cmd`, your editor, your git
  hooks): nightshift runs them as you asked, and they run with your rights;
- vulnerabilities in a dependency that nightshift does not make reachable
  (`govulncheck` runs on every pull request; report the dependency upstream);
- the security of the machine nightshift runs on (a local administrator, or
  malware running as you, can read what you can).

## How we keep nightshift secure

- Every pull request runs `govulncheck`, `gosec` (through golangci-lint) and
  the race detector on Linux, macOS and Windows; see
  [CONTRIBUTING.md](CONTRIBUTING.md).
- Release binaries are built by goreleaser in GitHub Actions from a tag on
  `main`, with the latest patched Go, and published with a `checksums.txt`.
- GitHub Actions are pinned by commit SHA and workflows run with least
  privilege; Dependabot proposes dependency and action updates weekly, and
  security updates as soon as they are published.
