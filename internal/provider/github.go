package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/michaelbeutler/nightshift/internal/config"
)

// gitHub opens pull requests on github.com, GitHub Enterprise Cloud
// (*.ghe.com) and GitHub Enterprise Server.
type gitHub struct {
	host   config.Host
	method string
	env    Env
}

func (g *gitHub) Kind() string { return "github" }
func (g *gitHub) Host() string { return g.host.Host }

func (g *gitHub) apiBase() string {
	if g.host.API != "" {
		return strings.TrimRight(g.host.API, "/")
	}
	switch {
	case g.host.Host == "github.com":
		return "https://api.github.com"
	case strings.HasSuffix(g.host.Host, ".ghe.com"):
		return "https://api." + g.host.Host
	}
	return "https://" + g.host.Host + "/api/v3"
}

func (g *gitHub) useCLI(ctx context.Context, dir string) (bool, error) {
	return pickCLI(ctx, g.env, g.method, "gh", dir, "auth", "status", "--hostname", g.host.Host)
}

func (g *gitHub) Check(ctx context.Context, dir string) Check {
	c := Check{Provider: "github", Host: g.host.Host}
	cli, err := g.useCLI(ctx, dir)
	if err != nil {
		c.Detail = err.Error()
		return c
	}
	if cli {
		c.OK, c.Via, c.Detail = true, "gh", "gh is installed and logged in to "+g.host.Host
		return c
	}
	tok, tried := findToken(ctx, g.env, "github", g.host, dir)
	if tok.value == "" {
		c.Detail = "no gh login and no token (tried " + strings.Join(tried, ", ") + ")"
		return c
	}
	c.OK, c.Via, c.Detail = true, "api", "REST API with a token from "+tok.source
	return c
}

// target is the repository the pull request is opened in, and head the
// branch reference: "branch", or "owner:branch" from a fork.
func (g *gitHub) target(req Request) (repo, head string) {
	repo, head = req.Remote.Path, req.Head
	if req.Target != "" && !strings.EqualFold(req.Target, req.Remote.Path) {
		repo, head = req.Target, req.Remote.Owner()+":"+req.Head
	}
	return repo, head
}

func (g *gitHub) CreatePR(ctx context.Context, req Request) (*Result, error) {
	cli, err := g.useCLI(ctx, req.Dir)
	if err != nil {
		return nil, err
	}
	if cli {
		return g.createCLI(ctx, req)
	}
	return g.createAPI(ctx, req)
}

func (g *gitHub) createCLI(ctx context.Context, req Request) (*Result, error) {
	repo, head := g.target(req)
	args := []string{"pr", "create", "--repo", g.host.Host + "/" + repo, "--head", head, "--title", req.Title, "--body-file", "-"}
	if req.Base != "" {
		args = append(args, "--base", req.Base)
	}
	if req.Draft {
		args = append(args, "--draft")
	}
	if len(req.Reviewers) > 0 {
		args = append(args, "--reviewer", joinList(req.Reviewers))
	}
	if len(req.Assignees) > 0 {
		args = append(args, "--assignee", joinList(req.Assignees))
	}
	if len(req.Labels) > 0 {
		args = append(args, "--label", joinList(req.Labels))
	}
	if req.Milestone != "" {
		args = append(args, "--milestone", req.Milestone)
	}
	out, errOut, err := g.env.Run(ctx, req.Dir, cliEnv, strings.NewReader(req.Body), "gh", args...)
	if err != nil {
		if strings.Contains(errOut, "already exists") {
			return &Result{URL: lastURL(errOut), Existing: true, Via: "gh"}, nil
		}
		return nil, cliError("gh pr create", errOut, out, err)
	}
	u := lastURL(out)
	return &Result{URL: u, Number: trailingNumber(u), Via: "gh"}, nil
}

func (g *gitHub) createAPI(ctx context.Context, req Request) (*Result, error) {
	tok, tried := findToken(ctx, g.env, "github", g.host, req.Dir)
	if tok.value == "" {
		return nil, fmt.Errorf("%w with %s: install gh and run `gh auth login`, or set GH_TOKEN (tried %s)", ErrNoAuth, g.host.Host, strings.Join(tried, ", "))
	}
	c := &apiClient{
		base:  g.apiBase(),
		token: tok.value,
		env:   g.env,
		headers: map[string]string{
			"Accept":               "application/vnd.github+json",
			"X-GitHub-Api-Version": "2022-11-28",
		},
	}
	repo, head := g.target(req)
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("GitHub repository %q is not owner/name", repo)
	}
	prefix := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)

	base := req.Base
	if base == "" {
		var r struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := c.do(ctx, http.MethodGet, prefix, nil, &r); err != nil {
			return nil, fmt.Errorf("look up the default branch: %w", err)
		}
		base = r.DefaultBranch
	}

	var pr struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	err := c.do(ctx, http.MethodPost, prefix+"/pulls", map[string]any{
		"title": req.Title, "body": req.Body, "head": head, "base": base, "draft": req.Draft,
	}, &pr)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity && strings.Contains(apiErr.Message, "already exists") {
		var open []struct {
			HTMLURL string `json:"html_url"`
			Number  int    `json:"number"`
		}
		q := "?state=open&head=" + url.QueryEscape(req.Remote.Owner()+":"+req.Head)
		if lerr := c.do(ctx, http.MethodGet, prefix+"/pulls"+q, nil, &open); lerr == nil && len(open) > 0 {
			return &Result{URL: open[0].HTMLURL, Number: open[0].Number, Existing: true, Via: "api"}, nil
		}
		return &Result{Existing: true, Via: "api"}, nil
	}
	if err != nil {
		return nil, err
	}

	// The pull request exists from here on. A failure to add reviewers or
	// labels is a warning: failing the job would open a second one on retry.
	res := &Result{URL: pr.HTMLURL, Number: pr.Number, Via: "api"}
	issue := prefix + "/issues/" + strconv.Itoa(pr.Number)
	var users, teams []string
	for _, r := range req.Reviewers {
		if _, team, ok := strings.Cut(r, "/"); ok {
			teams = append(teams, team)
		} else {
			users = append(users, r)
		}
	}
	if len(users)+len(teams) > 0 {
		body := map[string]any{"reviewers": nonNil(users), "team_reviewers": nonNil(teams)}
		if err := c.do(ctx, http.MethodPost, prefix+"/pulls/"+strconv.Itoa(pr.Number)+"/requested_reviewers", body, nil); err != nil {
			res.Warnings = append(res.Warnings, "reviewers: "+err.Error())
		}
	}
	patch := map[string]any{}
	if len(req.Labels) > 0 {
		patch["labels"] = req.Labels
	}
	if len(req.Assignees) > 0 {
		patch["assignees"] = req.Assignees
	}
	if req.Milestone != "" {
		if n, err := g.milestone(ctx, c, prefix, req.Milestone); err != nil {
			res.Warnings = append(res.Warnings, "milestone: "+err.Error())
		} else {
			patch["milestone"] = n
		}
	}
	if len(patch) > 0 {
		if err := c.do(ctx, http.MethodPatch, issue, patch, nil); err != nil {
			res.Warnings = append(res.Warnings, "labels/assignees/milestone: "+err.Error())
		}
	}
	return res, nil
}

// milestone resolves a milestone given by number or by title.
func (g *gitHub) milestone(ctx context.Context, c *apiClient, prefix, m string) (int, error) {
	if n, err := strconv.Atoi(m); err == nil {
		return n, nil
	}
	var all []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
	}
	if err := c.do(ctx, http.MethodGet, prefix+"/milestones?state=open&per_page=100", nil, &all); err != nil {
		return 0, err
	}
	for _, x := range all {
		if strings.EqualFold(x.Title, m) {
			return x.Number, nil
		}
	}
	return 0, fmt.Errorf("no open milestone titled %q", m)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func trailingNumber(u string) int {
	i := strings.LastIndexByte(u, '/')
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(u[i+1:])
	return n
}
