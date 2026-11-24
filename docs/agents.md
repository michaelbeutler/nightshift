# nightshift for AI agents

Coding agents commit and open pull requests the moment they finish. nightshift lets them
prepare everything the same way, and deliver it later. There are two ways in: the MCP
server, and the CLI with `--json`.

## MCP

`nightshift mcp` serves the tools over stdio (JSON-RPC, MCP revisions 2024-11-05 to
2025-11-25). Register it once:

```sh
# Claude Code
claude mcp add nightshift -- nightshift mcp

# Codex CLI
codex mcp add nightshift -- nightshift mcp
```

or in any client's JSON configuration (Cursor, Windsurf, Claude Desktop, …):

```json
{
  "mcpServers": {
    "nightshift": { "command": "nightshift", "args": ["mcp"] }
  }
}
```

| Tool | Does | Instead of |
| --- | --- | --- |
| `schedule_commit` | buffer the staged changes (plus `paths`, or `all`) as a commit made later | `git commit` |
| `adopt_commits` | turn unpushed plain commits into buffered ones | |
| `schedule_push` | push the branch after its buffered commits | `git push` |
| `schedule_pull_request` | open a PR/MR (title, body, reviewers, labels, draft, …) after the push, scheduling the push too | `gh pr create`, `glab mr create` |
| `branch_status` | what is planned for the branch, and what is staged but not buffered | |
| `list_jobs`, `show_job` | the queue | |
| `reschedule_job`, `cancel_job`, `retry_job` | change the plan | |
| `doctor` | can tonight's run push, sign and open pull requests? | |

Every tool takes `repo`, the absolute path of the repository (default: the server's
working directory), and the scheduling tools take `at` (`tonight`, `02:30`,
`tomorrow 1am`, `+2h`, `now`, `2026-12-01 02:00`). Results are JSON, also returned as
structured content. The server's instructions tell the model not to commit buffered
changes with git.

## CLI with `--json`

Every command prints JSON with `--json`; errors are `{"error": "…"}` with exit code 1.

```sh
nightshift --json commit -C "$REPO" -m "fix: handle empty input" -a
nightshift --json pr create -C "$REPO" --title "Fix empty input" --body-file /tmp/pr.md -r octocat
nightshift --json status -C "$REPO"
```

Without a terminal nightshift never opens an editor: pass `-m`/`-F` for commits and
`--body`/`--body-file`/`--fill` for pull requests.

## A snippet for your AGENTS.md or CLAUDE.md

```markdown
## Delivering work

Do not run `git commit`, `git push` or `gh pr create` / `glab mr create` yourself. Use
nightshift (MCP tools `schedule_commit`, `schedule_pull_request`, or the `nightshift` CLI
with `--json`), so the work is delivered in tonight's window. Write Conventional Commit
messages and a pull request body with a summary and a test plan. Check the plan with
`branch_status`. Never commit, stash or reset changes nightshift has buffered.
```
