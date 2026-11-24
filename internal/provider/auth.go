package provider

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/michaelbeutler/nightshift/internal/config"
	"github.com/michaelbeutler/nightshift/internal/git"
)

// token is an API token and where it came from. Only the source is ever
// printed.
type token struct {
	value  string
	source string
}

// findToken looks for an API token, in order: the host's token_cmd and
// token_env, the provider's usual environment variables, the token the CLI
// stored at login, and the git credential helper (osxkeychain, Git
// Credential Manager, libsecret, ...), which often holds one for https.
func findToken(ctx context.Context, env Env, kind string, h config.Host, dir string) (token, []string) {
	var tried []string
	if h.TokenCmd != "" {
		tried = append(tried, "token_cmd")
		if v := runTokenCmd(ctx, env, h.TokenCmd, dir); v != "" {
			return token{v, "token_cmd"}, tried
		}
	}
	if h.TokenEnv != "" {
		tried = append(tried, "$"+h.TokenEnv)
		if v := strings.TrimSpace(env.Getenv(h.TokenEnv)); v != "" {
			return token{v, "$" + h.TokenEnv}, tried
		}
	}
	for _, name := range envNames(kind, h.Host) {
		tried = append(tried, "$"+name)
		if v := strings.TrimSpace(env.Getenv(name)); v != "" {
			return token{v, "$" + name}, tried
		}
	}
	switch kind {
	case "github":
		tried = append(tried, "gh auth token")
		if v := cliOutput(ctx, env, dir, "gh", "auth", "token", "--hostname", h.Host); v != "" {
			return token{v, "gh auth token"}, tried
		}
	case "gitlab":
		tried = append(tried, "glab config")
		if v := cliOutput(ctx, env, dir, "glab", "config", "get", "token", "--host", h.Host); v != "" {
			return token{v, "glab config"}, tried
		}
	}
	tried = append(tried, "git credential helper")
	if v := credentialHelper(ctx, env, dir, h.Host); v != "" {
		return token{v, "git credential helper"}, tried
	}
	return token{}, tried
}

func envNames(kind, host string) []string {
	switch kind {
	case "github":
		if host == "github.com" || strings.HasSuffix(host, ".ghe.com") {
			return []string{"GH_TOKEN", "GITHUB_TOKEN"}
		}
		return []string{"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}
	case "gitlab":
		return []string{"GITLAB_TOKEN", "GL_TOKEN"}
	}
	return nil
}

func cliOutput(ctx context.Context, env Env, dir, tool string, args ...string) string {
	if _, err := env.LookPath(tool); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, _, err := env.Run(ctx, dir, cliEnv, nil, tool, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func runTokenCmd(ctx context.Context, env Env, command, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	shell, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	}
	out, _, err := env.Run(ctx, dir, nil, nil, shell, flag, command)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// credentialHelper asks git's credential helpers for the host's password
// without ever prompting.
func credentialHelper(ctx context.Context, env Env, dir, host string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	in := fmt.Sprintf("protocol=https\nhost=%s\n\n", host)
	out, _, err := env.Run(ctx, dir, git.NonInteractive, strings.NewReader(in), "git", "credential", "fill")
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(line, "password="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
