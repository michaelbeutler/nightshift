# Agent instructions

## Working on nightshift

The project's rules for every contributor, person or agent, are in [CLAUDE.md](CLAUDE.md)
and the files it links under [`.claude/rules/`](.claude/rules/). They apply to any coding
agent, whatever its tool reads by default; this file only points there so that each rule
has one home.

Read before changing code:

- [CLAUDE.md](CLAUDE.md): what nightshift is, "use what is installed", the invariants,
  commands and commits.
- [`.claude/rules/code-economy.md`](.claude/rules/code-economy.md): search before you
  write, one owner per rule, comments that say what is true now.
- [`.claude/rules/go-testing.md`](.claude/rules/go-testing.md): real git in temp repos, the
  fixture and fakes that exist, no network.
- [`.claude/rules/unattended.md`](.claude/rules/unattended.md): code that runs at night.
- [CONTRIBUTING.md](CONTRIBUTING.md): how to build, test and ship.

## Using nightshift from an agent

If you are an agent asked to *use* nightshift in some other repository, read
[docs/agents.md](docs/agents.md): connect `nightshift mcp`, or call the CLI with `--json`.
