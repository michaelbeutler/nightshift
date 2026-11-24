<!--
Thanks for contributing! Keep PRs focused. See CONTRIBUTING.md for conventions.

The PR title is a Conventional Commit, checked by CI, and becomes the merge
commit's subject and the line in the release notes:
  feat(schedule): pick a random time inside the night window
  fix(github)!: open pull requests as drafts by default
A change that makes existing users change their config, their commands or
what they can expect at night is breaking: mark it with `!` and describe the
migration under "Breaking changes".
-->

## What & why

<!-- What does this change and why? Link any issue: Closes #123 -->

## Type of change

- [ ] Bug fix
- [ ] Feature
- [ ] New provider (git host)
- [ ] Breaking change (config, commands, output or behaviour; `!` in the title, details below)
- [ ] Refactor / internal
- [ ] Docs / tooling / CI
- [ ] Dependency update

## Breaking changes

<!-- Delete if none. Otherwise: what a user has to change, before → after
(a config key, a flag, a command, what `--json` prints). -->

## What runs unattended

<!-- Does this change what nightshift does at night without anyone watching:
what it commits, how it signs, which credentials or token commands it uses,
which commands it runs, or what it pushes where (remote, ref, force)? If so,
say what and why it is safe. "No" is a valid answer, but say so explicitly. -->

## Tested how

<!-- Unit tests only, or also by hand: which host (GitHub, GitLab, self-managed),
which PR method (gh, glab, API), which signing setup, which OS. -->

## Checklist (see CONTRIBUTING.md → Definition of done)

- [ ] `just check` passes locally (fmt, lint, vet, race tests, govulncheck, tidy, build)
- [ ] New/changed behaviour has tests (real git in `t.TempDir()`, no network)
- [ ] The README / docs are updated if a command, flag or config key changed
- [ ] No token is printed, logged, written to a job file or put in an error message
- [ ] Commits and the title follow Conventional Commits (`just install-hooks` checks commits locally)
