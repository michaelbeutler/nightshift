# How nightshift works

nightshift splits "commit, push, open a pull request" into two halves: the part that needs
you (choosing what goes in, writing the message, passing the hooks) happens when you run
the command; the part that leaves a timestamp somewhere (the commit object, the push, the
pull request) happens at its scheduled time, done by a runner while you are away.

```mermaid
sequenceDiagram
    autonumber
    actor You
    participant NS as nightshift (CLI or MCP)
    participant Repo as your repository
    participant Q as queue (state dir)
    participant Runner as runner (every minute)
    participant Host as GitHub / GitLab

    You->>NS: nightshift commit -am "feat: …"
    NS->>Repo: git add -u, pre-commit + commit-msg hooks
    NS->>Repo: snapshot = commit-tree (unsigned), kept at refs/nightshift/commits/<id>
    NS->>Q: commit job at 01:36 (tree, message, author, parent)
    You->>NS: nightshift pr create --fill -r octocat
    NS->>Q: push job at 01:41, PR job at 01:51
    Note over You,Repo: you keep working; the changes stay staged
    Runner->>Q: 01:36 commit job is due
    Runner->>Repo: commit-tree -S (dated 01:36, signed now), update-ref feature (compare-and-swap)
    Runner->>Repo: 01:41 git push origin <sha>:refs/heads/feature
    Repo->>Host: push (your SSH key / credential helper)
    Runner->>Host: 01:51 gh pr create / glab mr create / REST API
```

## Buffering a commit

`nightshift commit` stages what you asked for (`-a`, paths), runs `pre-commit`, writes the
index as a tree, runs `prepare-commit-msg` and `commit-msg` on the message, and records:

- the **tree** and the **message**, exactly as they will be committed;
- the **author** and **committer** identities (from `git var`, so `includeIf` and
  `--author` apply);
- the **parent**: the branch's tip for the first buffered commit (`Base`), the previous
  buffered commit after that (`Parent`);
- a **snapshot**: an unsigned commit with the same tree and message, kept at
  `refs/nightshift/commits/<id>`. It keeps the tree safe from `git gc` and lets
  `nightshift show` and `git show` display it. It is never pushed.

Nothing else changes: the branch stays where it was, and the index and working tree keep
your changes (still staged), so you can keep working. Each buffered commit is a snapshot of
the whole index, so the next one contains everything staged since, and its diff against
the previous snapshot is exactly what you added.

`--amend` replaces the last buffered commit. `adopt` turns commits you already made with
plain `git commit` (and did not push) into buffered commits: the branch moves back like
`git reset --soft`, and each commit is recreated at night with the same tree, message and
author.

## Planning a branch

Every job has a time (`At`) and the jobs it waits for (`DependsOn`). For one branch:

```
commit A ──▶ commit B ──▶ push ──▶ pull request
 01:36        01:47       01:52     02:03
```

- With no `--at`, the first job goes to a random time in `schedule.window`, and each job
  that follows goes a random `schedule.spacing` after the one it waits for, so a series of
  commits looks like a series of commits.
- `--at` with an exact date and time (`2026-12-01 02:00:30`, `dec 1 2am`, `+2h`, with an
  optional zone) is kept to the second; it is refused, not moved, when it is not after the
  job it waits for. A loose time (`tonight`, `now`, a window, a day) lets nightshift pick
  the moment. A pull request given an exact time pulls the push it plans to just before
  it.
- A job never runs before the jobs it waits for. Buffering another commit moves a pending
  push (and the pull request after it) behind it, and the push then carries it.
- `reschedule` moves a job and everything that waits for it by the same amount;
  `reschedule <first commit> now` ships a whole branch on the runner's next pass.
- `cancel` on a buffered commit also cancels the commits after it (their snapshots contain
  its changes). A push that carried them carries the commit before instead, or is canceled
  with its pull request when nothing is left. Your files are never touched.

Jobs are JSON files in the state directory (`nightshift config path`), one per job,
written atomically with mode `0600`. A lock serializes changes to them.

## The runner

`nightshift run` takes the runner lock (only one works at a time), then repeatedly picks
the earliest job whose time has come and whose dependencies are done, and runs it:

- **commit**: `git commit-tree` with the buffered tree, message and identities, the
  author and committer dates set to the job's time, and `-S` when signing is on
  (`sign: auto` follows `commit.gpgSign`). Then `git update-ref` moves the branch from the
  commit it expects to the new one, compare-and-swap. If the branch moved in the meantime
  the job fails; nothing is forced. If the branch is checked out and its index was left at
  the old commit (after a `git stash`, say), the working tree is fast-forwarded with
  `git read-tree -m -u`, which never overwrites local changes.
- **push**: `git push <remote> <sha>:refs/heads/<branch>`, run by your git with your
  configuration, then the upstream is set if there was none.
- **pr**: the provider for the remote's host opens it (see
  [authentication.md](authentication.md)).

A failed push or pull request is retried with a backoff (2, 10, 30 minutes, up to
`runner.retries`); a failed commit needs a person and fails at once. Failures show up in
`nightshift list` and `status`, and as a desktop notification (`notify`).

### When the machine was asleep

The OS service runs `nightshift run` every minute and right after the machine wakes up.
Jobs whose time passed run then, in order, and **commits keep their scheduled dates**: a
commit planned for 01:36 is dated 01:36 even if it is created at 07:50. The push and the
pull request get the time they actually happen, because the git host records those itself.
With `schedule.missed: next-window`, jobs more than `schedule.grace` late move to the next
night instead.

On macOS you can have the machine wake for the window, e.g.
`sudo pmset repeat wakeorpoweron MTWRFSU 01:00:00`.

## What the git host shows

| | What sets it | What it shows |
| --- | --- | --- |
| Commit author and committer date | nightshift, at the scheduled time | the night |
| Commit signature | your key, at night | verified, as usual |
| Push time, "pushed" events | the host, when the push arrives | when the runner pushed |
| Pull request created at | the host, when it is opened | when the runner opened it |

That is why nightshift does the push and the pull request at night rather than just dating
the commits: it is the only way they say the night as well.

## Things to know

- **Buffered changes stay staged.** That is how you keep working on them. Do not commit
  them with plain `git commit` (the branch would move under the buffered commits and the
  runner would refuse them); `nightshift status` shows what is buffered and what is staged
  beyond it.
- **`refs/nightshift/*` are local.** Normal pushes never send them; `git push --mirror`
  would.
- **One machine, one queue.** The queue lives in your state directory, so the machine that
  buffered the work is the one that delivers it.
