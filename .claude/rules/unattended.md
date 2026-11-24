# Code that runs unattended

Enforced in review. Applies to everything the runner reaches: `engine.Run` and what it
calls (`runCommit`, `runPush`, `runPR`, `internal/provider`, `internal/git`), and to
`doctor`, which must fail the way the night would.

The runner works at three in the morning with nobody at the keyboard, often from launchd
or systemd with a minimal environment. A prompt it cannot answer, a hang, or a guess made
on someone's behalf is a failure that is only discovered the next day.

## 1. Nothing waits for a person

- Every external command gets a context with a deadline (`commitTimeout`, `pushTimeout`,
  `prTimeout` in `internal/engine/run.go`, the per-call timeouts in `internal/provider`).
  A signing program waiting for a passphrase must time out, not hang the runner.
- Git runs with `git.NonInteractive` (`GIT_TERMINAL_PROMPT=0`, `GCM_INTERACTIVE=never`);
  `gh` and `glab` with `provider.cliEnv` (no prompts, pager, color or update checks). No
  command gets the terminal's stdin.
- A check that predicts the night (`doctor`) runs detached from the terminal
  (`git.Exec{Detach: true}`), so an SSH passphrase prompt fails there too.

## 2. Never guess, never force

- The runner moves a branch only from the commit it expects (`UpdateRef` with `check`).
  When the branch moved, the job fails with `BranchMovedError`; it is not rebased, merged
  or forced. `--force-with-lease` happens only when the user asked for it.
- A commit is created from the snapshot's tree and message exactly; nothing is re-run or
  re-generated at night (hooks ran when the commit was buffered).
- Every step is safe to run twice. A crash between creating a commit and recording it is
  recovered by recognising the commit already on the branch (`runCommit`); a pushed branch
  pushes again as a no-op; an existing pull request is reported, not duplicated.

## 3. Failures are recorded, retried only when retrying can help

- A failed push or pull request goes back in the queue with a backoff (`finish`,
  `backoff`) up to `runner.retries`; a failed commit fails at once, because what breaks
  it (a moved branch, a key that needs a passphrase) needs a person.
- Once a pull request exists, a later problem (a reviewer that does not exist, a label the
  token may not set) is a warning in the job's result, never a failure that would open a
  second pull request on retry.
- The error saved in a job, logged and notified says what to do next, in one line, and
  never contains a token (`provider.clean`, `cliError`, `APIError`).

## 4. The environment is not the terminal's

- Assume a minimal `PATH`, no `SSH_AUTH_SOCK` beyond what launchd or systemd provides, and
  no terminal. `service.CaptureEnv` decides what the service inherits; a variable the
  runner needs is added there, with the reason.
- Paths come from the job (`Job.Repo`) and the config, never from the working directory.
