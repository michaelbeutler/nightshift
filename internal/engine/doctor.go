package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/michaelbeutler/nightshift/internal/git"
	"github.com/michaelbeutler/nightshift/internal/provider"
	"github.com/michaelbeutler/nightshift/internal/queue"
)

// Check levels.
const (
	LevelOK   = "ok"
	LevelWarn = "warn"
	LevelFail = "fail"
)

// Check is one finding of `nightshift doctor`.
type Check struct {
	Name   string `json:"name"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// ServiceStatus reports whether the background runner is installed.
type ServiceStatus func(ctx context.Context) (installed bool, detail string)

// Doctor checks that what was buffered today can run unattended tonight:
// git itself, the background runner, and, for the repository at dir, the
// identity, push access, signing and pull request authentication, each
// tried the way the runner will try it, without a terminal to prompt on.
func (e *Engine) Doctor(ctx context.Context, dir string, service ServiceStatus) []Check {
	var checks []Check
	add := func(name, level, detail, hint string) {
		checks = append(checks, Check{Name: name, Level: level, Detail: detail, Hint: hint})
	}

	v, raw, err := git.Version(ctx)
	switch {
	case err != nil:
		add("git", LevelFail, "git is not installed or not on PATH", "install git 2.36 or later")
		return checks
	case v[0] < 2 || (v[0] == 2 && v[1] < 36):
		add("git", LevelFail, "git "+raw+" is too old", "nightshift needs git 2.36 or later (for git hook run)")
	default:
		add("git", LevelOK, "git "+raw, "")
	}

	if service != nil {
		if ok, detail := service(ctx); ok {
			add("runner", LevelOK, detail, "")
		} else {
			add("runner", LevelWarn, detail, "nothing runs at night until you run `nightshift service install` (or keep `nightshift daemon` running)")
		}
	}

	if all, err := e.Store.All(); err == nil {
		var pending, failed int
		for _, j := range all {
			switch j.Status {
			case queue.StatusPending, queue.StatusRunning:
				pending++
			case queue.StatusFailed:
				failed++
			}
		}
		level, hint := LevelOK, ""
		if failed > 0 {
			level, hint = LevelWarn, "see `nightshift list`, then `nightshift retry <id>` or `nightshift cancel <id>`"
		}
		add("queue", level, fmt.Sprintf("%d pending, %d failed (state in %s)", pending, failed, e.Store.Dir()), hint)
	}

	repo, err := git.Open(ctx, dir)
	if err != nil {
		add("repository", LevelWarn, "not inside a git repository; skipped the repository checks", "run nightshift doctor inside a repository")
		return checks
	}
	if id, err := repo.Var(ctx, "GIT_COMMITTER_IDENT"); err != nil {
		add("identity", LevelFail, "git does not know who you are", "set user.name and user.email")
	} else if who, err := git.ParseIdent(id); err == nil {
		add("identity", LevelOK, who.String(), "")
	}

	branch, _ := repo.Branch(ctx)
	remote := repo.PushRemote(ctx, branch, e.Config.Remote)
	raw, err = repo.RemoteURL(ctx, remote)
	if err != nil {
		add("remote", LevelFail, fmt.Sprintf("remote %q does not exist", remote), "add it with git remote add, or set remote in the config")
		return checks
	}
	u, _ := git.ParseRemoteURL(raw)
	checks = append(checks, e.checkPush(ctx, repo, remote, u))

	if prov, err := provider.For(ctx, u, e.Config.Providers, e.Config.PR.Method, e.Env); err != nil {
		add("pull requests", LevelWarn, err.Error(), "pushing still works; only pull requests need a known host")
	} else {
		c := prov.Check(ctx, repo.Dir)
		if c.OK {
			add("pull requests", LevelOK, fmt.Sprintf("%s via %s", c.Provider, c.Detail), "")
		} else {
			add("pull requests", LevelFail, fmt.Sprintf("%s on %s: %s", c.Provider, c.Host, c.Detail), installHint(c.Provider))
		}
	}

	checks = append(checks, e.checkSigning(ctx, repo)...)
	return checks
}

func installHint(kind string) string {
	if kind == "gitlab" {
		return "install glab and run `glab auth login`, or set GITLAB_TOKEN (or token_cmd in the config) where the runner can see it"
	}
	return "install gh and run `gh auth login`, or set GH_TOKEN (or token_cmd in the config) where the runner can see it"
}

// checkPush dry-runs a push without a terminal, so an SSH key that needs a
// passphrase or an https remote with no stored credentials shows up now.
func (e *Engine) checkPush(ctx context.Context, repo *git.Repo, remote string, u git.RemoteURL) Check {
	env := append([]string(nil), git.NonInteractive...)
	if u.Scheme == "ssh" {
		base := os.Getenv("GIT_SSH_COMMAND")
		if base == "" {
			base, _ = repo.Config(ctx, "core.sshCommand")
		}
		if base == "" {
			base = "ssh"
		}
		env = append(env, "GIT_SSH_COMMAND="+base+" -o BatchMode=yes")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	args := []string{"push", "--dry-run", "--no-verify", "--porcelain", remote, "HEAD:refs/heads/nightshift-doctor-probe"}
	if _, ok := repo.TryResolve(ctx, "HEAD"); !ok {
		args = []string{"ls-remote", remote, "HEAD"}
	}
	if _, err := repo.Do(ctx, git.Exec{Args: args, Env: env, Detach: true}); err != nil {
		hint := "make sure pushing works without typing anything: an SSH key in ssh-agent (ssh-add --apple-use-keychain on macOS), or a credential helper for https"
		return Check{Name: "push", Level: LevelFail, Detail: fmt.Sprintf("cannot reach %s (%s) unattended: %s", remote, u, firstLine(err.Error())), Hint: hint}
	}
	return Check{Name: "push", Level: LevelOK, Detail: fmt.Sprintf("%s (%s) accepts pushes without a prompt", remote, u)}
}

// checkSigning signs a throwaway commit without a terminal, the way the
// runner signs at night, and adds advice for the usual key setups.
func (e *Engine) checkSigning(ctx context.Context, repo *git.Repo) []Check {
	if !e.shouldSign(ctx, repo, "") {
		return []Check{{Name: "signing", Level: LevelOK, Detail: "commits are not signed (commit.gpgSign is off)"}}
	}
	format, _ := repo.Config(ctx, "gpg.format")
	if format == "" {
		format = "openpgp"
	}
	key, _ := repo.Config(ctx, "user.signingKey")
	program, _ := repo.Config(ctx, "gpg."+format+".program")
	var checks []Check

	empty, err := repo.EmptyTree(ctx)
	if err == nil {
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err = repo.Do(tctx, git.Exec{
			Args:   []string{"commit-tree", "-S", empty, "-m", "nightshift doctor: signing test"},
			Detach: true,
		})
		cancel()
	}
	if err != nil {
		checks = append(checks, Check{
			Name: "signing", Level: LevelFail,
			Detail: fmt.Sprintf("%s signing failed without a terminal: %s", format, firstLine(err.Error())),
			Hint:   signingHint(format),
		})
	} else {
		checks = append(checks, Check{Name: "signing", Level: LevelOK, Detail: fmt.Sprintf("%s signing works without a terminal", format)})
	}

	switch {
	case strings.Contains(program, "op-ssh-sign"):
		checks = append(checks, Check{
			Name: "signing", Level: LevelWarn,
			Detail: "1Password signs your commits; it must be unlocked and allow signing without a prompt at night",
			Hint:   "in 1Password, keep the app unlocked overnight or sign with a key held by ssh-agent instead",
		})
	case format == "ssh":
		if c, ok := sshKeyCheck(ctx, key); ok {
			checks = append(checks, c)
		}
	case format == "openpgp":
		checks = append(checks, Check{
			Name: "signing", Level: LevelWarn,
			Detail: "gpg-agent forgets a cached passphrase after default-cache-ttl (10 minutes unless configured)",
			Hint:   "set default-cache-ttl and max-cache-ttl in gpg-agent.conf to cover the night, or sign with an SSH key held by ssh-agent",
		})
	}
	return checks
}

func signingHint(format string) string {
	switch format {
	case "ssh":
		return "load the key into ssh-agent (ssh-add --apple-use-keychain ~/.ssh/id_ed25519 on macOS) so signing needs no passphrase"
	case "x509":
		return "the S/MIME key must be usable without a PIN prompt"
	}
	return "unlock the key in gpg-agent and raise its cache TTL, or use an SSH signing key in ssh-agent"
}

// sshKeyCheck says whether an SSH signing key can sign unattended: held by
// ssh-agent, or a key file without a passphrase.
func sshKeyCheck(ctx context.Context, key string) (Check, bool) {
	if key == "" || strings.HasPrefix(key, "key::") || strings.HasPrefix(key, "ssh-") {
		return Check{}, false // a literal public key: only an agent can sign with it
	}
	if strings.HasPrefix(key, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			key = filepath.Join(home, key[2:])
		}
	}
	pub, priv := key, strings.TrimSuffix(key, ".pub")
	if !strings.HasSuffix(key, ".pub") {
		pub = key + ".pub"
	}
	pubData, err := os.ReadFile(pub)
	if err == nil {
		fields := strings.Fields(string(pubData))
		if len(fields) >= 2 {
			if out, err := runQuiet(ctx, "ssh-add", "-L"); err == nil && strings.Contains(out, fields[1]) {
				return Check{Name: "signing", Level: LevelOK, Detail: "the SSH signing key is loaded in ssh-agent"}, true
			}
		}
	}
	if _, err := runQuiet(ctx, "ssh-keygen", "-y", "-P", "", "-f", priv); err == nil {
		return Check{Name: "signing", Level: LevelOK, Detail: "the SSH signing key has no passphrase"}, true
	}
	return Check{
		Name: "signing", Level: LevelWarn,
		Detail: "the SSH signing key has a passphrase and is not loaded in ssh-agent right now",
		Hint:   "run ssh-add (macOS: ssh-add --apple-use-keychain " + priv + ") so the runner can sign at night",
	}, true
}

// runQuiet runs a helper tool with no input and a short deadline.
func runQuiet(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}
