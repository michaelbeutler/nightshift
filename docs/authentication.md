# Authentication and signing

nightshift has no credentials of its own. It runs your git and your tools, and finds
tokens the way they do, at the moment it needs one. What matters is that it all works
**without a prompt**, because at night nobody is there to type a passphrase.
`nightshift doctor` checks each part the way the runner will, detached from your terminal.

## Pushing

Pushes are `git push`, so everything git supports works:

| Remote | What authenticates | Unattended when |
| --- | --- | --- |
| `git@host:owner/repo.git`, `ssh://…` | your SSH key, through ssh-agent or the key file | the key is loaded in an agent that stays unlocked (macOS: `ssh-add --apple-use-keychain ~/.ssh/id_ed25519`, with `UseKeychain yes` in `~/.ssh/config`), or has no passphrase |
| `https://…` | your credential helper: osxkeychain, Git Credential Manager, libsecret, `gh auth setup-git`, `glab auth setup-git`, a stored PAT | the helper answers without asking |
| SSH aliases (`Host work` in `~/.ssh/config`) | as configured there (`IdentityFile`, `IdentityAgent`) | as above |
| `insteadOf` / `pushInsteadOf` rewrites | applied by git as usual | as above |

The runner sets `GIT_TERMINAL_PROMPT=0` and `GCM_INTERACTIVE=never`, so a missing
credential fails the push quickly instead of waiting. Your `pre-push` hooks run.

The background service starts with your `PATH` and, where it is stable across reboots,
your `SSH_AUTH_SOCK` (1Password, gnome-keyring, a systemd user agent). On macOS, launchd
gives the agent its own `SSH_AUTH_SOCK`, which reaches the system ssh-agent and the keys
added with `--apple-use-keychain`.

## Signing

The commits nightshift creates are signed at night with your configured key when
`commit.gpgSign` is true (or `sign: always`; `--gpg-sign`/`--no-gpg-sign` per commit).
Whatever `gpg.format` says is used:

| `gpg.format` | Works unattended when | Notes |
| --- | --- | --- |
| `ssh` (key file) | the key is in ssh-agent, or has no passphrase | `doctor` checks both |
| `ssh` with 1Password (`op-ssh-sign`) | 1Password is unlocked and does not ask to approve | it locks on sleep by default; check its settings |
| `openpgp` (gpg) | gpg-agent still has the passphrase cached | set `default-cache-ttl` and `max-cache-ttl` in `gpg-agent.conf` to cover the night, or use a dedicated signing subkey |
| `x509` (gpgsm, smimesign) | the key needs no PIN | |
| hardware keys (YubiKey touch, PIN) | not at night | use an SSH or GPG key that does not need a touch for nightshift's commits |

The buffered snapshot is never signed; only the real commit is, when it is made. A
signing failure fails the commit job with the reason, and nothing is pushed.

## Opening pull requests

nightshift works out the provider from the remote's host: `github.com`, `*.ghe.com` and
hosts containing "github" are GitHub; `gitlab.com` and hosts containing "gitlab" are
GitLab; SSH aliases are resolved with `ssh -G`. Anything else goes in the config:

```yaml
providers:
  - host: git.example.com
    type: gitlab                          # github | gitlab
    api: https://git.example.com/api/v4   # optional
    token_env: EXAMPLE_GITLAB_TOKEN       # optional
    token_cmd: op read op://dev/gitlab/token   # optional
```

With `pr.method: auto` (the default) it uses the official CLI when it is installed and
logged in to the host, and the REST API otherwise:

| | GitHub | GitLab |
| --- | --- | --- |
| CLI | `gh pr create` (checked with `gh auth status --hostname`) | `glab mr create` (checked with `glab auth status --hostname`) |
| API | `POST /repos/{owner}/{repo}/pulls`, then reviewers, labels, assignees, milestone | `POST /projects/{path}/merge_requests` with reviewer, assignee and milestone IDs |
| Draft | `--draft` / `"draft": true` | `--draft` / a `Draft:` title |
| Reviewers | users and `org/team` | usernames |

`pr.method: cli` insists on the CLI; `pr.method: api` always uses the API.

### Where the API token comes from

The first of these that has one, looked up when the pull request is opened and never
stored:

1. the host's `token_cmd` (a command that prints the token: `op read …`,
   `security find-generic-password -w …`, `pass show …`),
2. the host's `token_env`,
3. `GH_TOKEN`, `GITHUB_TOKEN` (github.com and `*.ghe.com`), `GH_ENTERPRISE_TOKEN`,
   `GITHUB_ENTERPRISE_TOKEN` (GitHub Enterprise Server), `GITLAB_TOKEN`, `GL_TOKEN`,
4. the CLI's stored login: `gh auth token --hostname`, `glab config get token --host`,
5. your git credential helper (`git credential fill` for `https://<host>`), which often
   holds a token already.

Remember that the service does not see variables exported in your shell profile;
`token_cmd` or the CLI's login are the reliable choices for the runner.

### Forks

`nightshift pr create --target-repo upstream-owner/repo` opens the pull request in another
GitHub repository with `owner:branch` as the head. Merge requests across GitLab projects
are not supported yet.
