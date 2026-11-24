// Package provider opens pull requests (GitHub) and merge requests (GitLab).
//
// Each provider prefers the official CLI the user already has (gh, glab),
// with its login, its keyring and its enterprise hosts, and falls back to the
// REST API with a token found the way the user's tools find one: a
// configured command or variable, the CLI's stored token, or the git
// credential helper.
package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/michaelbeutler/nightshift/internal/config"
	"github.com/michaelbeutler/nightshift/internal/git"
)

var (
	// ErrUnknownHost is returned for a remote host whose provider cannot be told.
	ErrUnknownHost = errors.New("unknown git host")
	// ErrLocalRemote is returned for a remote that is a path on this machine.
	ErrLocalRemote = errors.New("the remote is a local path, which has no pull requests")
	// ErrNoAuth is returned when neither the CLI nor a token is available.
	ErrNoAuth = errors.New("no way to authenticate")
)

// Request is a pull request to open.
type Request struct {
	// Dir is the working tree, where gh and glab run.
	Dir string
	// Remote is the remote the branch was pushed to.
	Remote git.RemoteURL
	// Target is the repository path to open it in, when that differs from
	// Remote's (a pull request from a fork); empty means Remote's.
	Target    string
	Head      string
	Base      string
	Title     string
	Body      string
	Draft     bool
	Reviewers []string
	Assignees []string
	Labels    []string
	Milestone string
}

// Result is an opened (or already existing) pull request.
type Result struct {
	URL      string   `json:"url"`
	Number   int      `json:"number,omitempty"`
	Existing bool     `json:"existing,omitempty"`
	Via      string   `json:"via"`
	Warnings []string `json:"warnings,omitempty"`
}

// Check describes how a provider would authenticate, for `nightshift doctor`.
type Check struct {
	Provider string `json:"provider"`
	Host     string `json:"host"`
	Via      string `json:"via,omitempty"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
}

// Provider opens pull requests on one host.
type Provider interface {
	Kind() string
	Host() string
	CreatePR(ctx context.Context, req Request) (*Result, error)
	Check(ctx context.Context, dir string) Check
}

// Env is the outside world a provider uses, so tests can replace it.
type Env struct {
	HTTP      *http.Client
	LookPath  func(file string) (string, error)
	Run       func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) (stdout, stderr string, err error)
	Getenv    func(key string) string
	UserAgent string
}

// DefaultEnv uses the real network, PATH and environment.
func DefaultEnv(userAgent string) Env {
	return Env{
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		LookPath:  exec.LookPath,
		Run:       runCommand,
		Getenv:    os.Getenv,
		UserAgent: userAgent,
	}
}

func runCommand(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// For returns the provider for a remote, given the configured hosts and the
// configured method ("auto", "cli" or "api").
func For(ctx context.Context, remote git.RemoteURL, hosts []config.Host, method string, env Env) (Provider, error) {
	kind, h, err := Detect(ctx, remote, hosts, env)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "github":
		return &gitHub{host: h, method: method, env: env}, nil
	case "gitlab":
		return &gitLab{host: h, method: method, env: env}, nil
	}
	return nil, fmt.Errorf("%w: %s (type %q)", ErrUnknownHost, remote.Host, kind)
}

// Detect works out which provider serves a remote: a configured host
// first, then the host's name, then, for an SSH alias from ~/.ssh/config,
// the name ssh resolves it to. The returned Host carries the real hostname.
func Detect(ctx context.Context, remote git.RemoteURL, hosts []config.Host, env Env) (string, config.Host, error) {
	if remote.IsLocal() {
		return "", config.Host{}, ErrLocalRemote
	}
	if kind, h, ok := detect(remote.Host, hosts); ok {
		return kind, h, nil
	}
	if remote.Scheme == "ssh" {
		if real := sshHostname(ctx, env, remote.Host); real != "" && !strings.EqualFold(real, remote.Host) {
			if kind, h, ok := detect(real, hosts); ok {
				return kind, h, nil
			}
		}
	}
	return "", config.Host{}, fmt.Errorf("%w %q: add it to the config under providers (type github or gitlab)", ErrUnknownHost, remote.Host)
}

func detect(host string, hosts []config.Host) (string, config.Host, bool) {
	host = strings.ToLower(host)
	for _, h := range hosts {
		if strings.EqualFold(h.Host, host) {
			return h.Type, h, true
		}
	}
	switch {
	case host == "ssh.github.com": // GitHub's SSH-over-443 endpoint
		return "github", config.Host{Host: "github.com", Type: "github"}, true
	case host == "altssh.gitlab.com":
		return "gitlab", config.Host{Host: "gitlab.com", Type: "gitlab"}, true
	case host == "github.com", strings.HasSuffix(host, ".ghe.com"), strings.Contains(host, "github"):
		return "github", config.Host{Host: host, Type: "github"}, true
	case host == "gitlab.com", strings.Contains(host, "gitlab"):
		return "gitlab", config.Host{Host: host, Type: "gitlab"}, true
	}
	return "", config.Host{}, false
}

// sshHostname asks ssh what an alias such as "work" (Host work / HostName
// github.com in ~/.ssh/config) really points at.
func sshHostname(ctx context.Context, env Env, alias string) string {
	if strings.HasPrefix(alias, "-") {
		return ""
	}
	if _, err := env.LookPath("ssh"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _, err := env.Run(ctx, "", nil, nil, "ssh", "-G", alias)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(line, "hostname "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

var (
	urlRe   = regexp.MustCompile(`https?://[^\s"'<>]+`)
	ctrlRe  = regexp.MustCompile(`\x1b\[[0-9;?]*[\x20-\x2f]*[\x40-\x7e]|[\x00-\x08\x0b-\x1f\x7f]`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// lastURL finds the last http(s) URL in a tool's output.
func lastURL(s string) string {
	all := urlRe.FindAllString(s, -1)
	if len(all) == 0 {
		return ""
	}
	return strings.TrimRight(all[len(all)-1], ".,)")
}

// clean makes text from a server or tool safe and short for a terminal:
// no escape sequences, one line, at most n runes.
func clean(s string, n int) string {
	s = ctrlRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}

// cliError turns a failed gh or glab run into an error with its message.
func cliError(tool, stderr, stdout string, err error) error {
	msg := clean(stderr, 400)
	if msg == "" {
		msg = clean(stdout, 400)
	}
	if msg == "" {
		return fmt.Errorf("%s: %w", tool, err)
	}
	return fmt.Errorf("%s: %s", tool, msg)
}

// pickCLI decides whether to use the CLI: "cli" insists on it, "api" never
// uses it, and "auto" uses it when installed and logged in to the host.
func pickCLI(ctx context.Context, env Env, method, tool, dir string, statusArgs ...string) (bool, error) {
	switch method {
	case "api":
		return false, nil
	case "cli":
		if _, err := env.LookPath(tool); err != nil {
			return false, fmt.Errorf("pr.method is cli, but %s is not installed: %w", tool, err)
		}
		return true, nil
	}
	if _, err := env.LookPath(tool); err != nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, _, err := env.Run(ctx, dir, cliEnv, nil, tool, statusArgs...)
	return err == nil, nil
}

// cliEnv keeps gh and glab from prompting, paging, coloring or checking for
// updates.
var cliEnv = []string{
	"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_SPINNER_DISABLED=1",
	"NO_PROMPT=1", "GLAB_CHECK_UPDATE=false",
	"NO_COLOR=1", "PAGER=cat", "GH_PAGER=cat",
}

func joinList(v []string) string { return strings.Join(v, ",") }
