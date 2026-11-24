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

// gitLab opens merge requests on gitlab.com and self-managed GitLab.
type gitLab struct {
	host   config.Host
	method string
	env    Env
}

func (g *gitLab) Kind() string { return "gitlab" }
func (g *gitLab) Host() string { return g.host.Host }

func (g *gitLab) apiBase() string {
	if g.host.API != "" {
		return strings.TrimRight(g.host.API, "/")
	}
	return "https://" + g.host.Host + "/api/v4"
}

func (g *gitLab) useCLI(ctx context.Context, dir string) (bool, error) {
	return pickCLI(ctx, g.env, g.method, "glab", dir, "auth", "status", "--hostname", g.host.Host)
}

func (g *gitLab) Check(ctx context.Context, dir string) Check {
	c := Check{Provider: "gitlab", Host: g.host.Host}
	cli, err := g.useCLI(ctx, dir)
	if err != nil {
		c.Detail = err.Error()
		return c
	}
	if cli {
		c.OK, c.Via, c.Detail = true, "glab", "glab is installed and logged in to "+g.host.Host
		return c
	}
	tok, tried := findToken(ctx, g.env, "gitlab", g.host, dir)
	if tok.value == "" {
		c.Detail = "no glab login and no token (tried " + strings.Join(tried, ", ") + ")"
		return c
	}
	c.OK, c.Via, c.Detail = true, "api", "REST API with a token from "+tok.source
	return c
}

func (g *gitLab) CreatePR(ctx context.Context, req Request) (*Result, error) {
	if req.Target != "" && !strings.EqualFold(req.Target, req.Remote.Path) {
		return nil, errors.New("merge requests from a fork into another project are not supported yet; see CONTRIBUTING.md")
	}
	cli, err := g.useCLI(ctx, req.Dir)
	if err != nil {
		return nil, err
	}
	if cli {
		return g.createCLI(ctx, req)
	}
	return g.createAPI(ctx, req)
}

func (g *gitLab) createCLI(ctx context.Context, req Request) (*Result, error) {
	args := []string{
		"mr", "create", "--yes",
		"--repo", "https://" + g.host.Host + "/" + req.Remote.Path,
		"--source-branch", req.Head,
		"--title", req.Title,
		"--description", req.Body,
	}
	if req.Base != "" {
		args = append(args, "--target-branch", req.Base)
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
	out, errOut, err := g.env.Run(ctx, req.Dir, cliEnv, nil, "glab", args...)
	if err != nil {
		if strings.Contains(errOut+out, "already exists") {
			return &Result{URL: lastURL(errOut + "\n" + out), Existing: true, Via: "glab"}, nil
		}
		return nil, cliError("glab mr create", errOut, out, err)
	}
	u := lastURL(out)
	return &Result{URL: u, Number: trailingNumber(u), Via: "glab"}, nil
}

func (g *gitLab) createAPI(ctx context.Context, req Request) (*Result, error) {
	tok, tried := findToken(ctx, g.env, "gitlab", g.host, req.Dir)
	if tok.value == "" {
		return nil, fmt.Errorf("%w with %s: install glab and run `glab auth login`, or set GITLAB_TOKEN (tried %s)", ErrNoAuth, g.host.Host, strings.Join(tried, ", "))
	}
	c := &apiClient{base: g.apiBase(), token: tok.value, env: g.env}
	// GitLab takes the URL-encoded path as the project ID: group%2Fsub%2Fproject.
	project := "/projects/" + strings.ReplaceAll(url.PathEscape(req.Remote.Path), "/", "%2F")

	base := req.Base
	if base == "" {
		var p struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := c.do(ctx, http.MethodGet, project, nil, &p); err != nil {
			return nil, fmt.Errorf("look up the default branch: %w", err)
		}
		base = p.DefaultBranch
	}

	res := &Result{Via: "api"}
	body := map[string]any{
		"source_branch": req.Head,
		"target_branch": base,
		"title":         req.Title,
		"description":   req.Body,
	}
	if req.Draft && !strings.HasPrefix(strings.ToLower(req.Title), "draft:") {
		body["title"] = "Draft: " + req.Title
	}
	if len(req.Labels) > 0 {
		body["labels"] = joinList(req.Labels)
	}
	if ids := g.userIDs(ctx, c, req.Reviewers, res); len(ids) > 0 {
		body["reviewer_ids"] = ids
	}
	if ids := g.userIDs(ctx, c, req.Assignees, res); len(ids) > 0 {
		body["assignee_ids"] = ids
	}
	if req.Milestone != "" {
		if id, err := g.milestone(ctx, c, project, req.Milestone); err != nil {
			res.Warnings = append(res.Warnings, "milestone: "+err.Error())
		} else {
			body["milestone_id"] = id
		}
	}

	var mr struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}
	err := c.do(ctx, http.MethodPost, project+"/merge_requests", body, &mr)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		var open []struct {
			IID    int    `json:"iid"`
			WebURL string `json:"web_url"`
		}
		q := "?state=opened&source_branch=" + url.QueryEscape(req.Head)
		if lerr := c.do(ctx, http.MethodGet, project+"/merge_requests"+q, nil, &open); lerr == nil && len(open) > 0 {
			return &Result{URL: open[0].WebURL, Number: open[0].IID, Existing: true, Via: "api"}, nil
		}
		return &Result{Existing: true, Via: "api"}, nil
	}
	if err != nil {
		return nil, err
	}
	res.URL, res.Number = mr.WebURL, mr.IID
	return res, nil
}

// userIDs resolves usernames to the numeric IDs the API wants; an unknown
// user is a warning, not a failure.
func (g *gitLab) userIDs(ctx context.Context, c *apiClient, names []string, res *Result) []int {
	var ids []int
	for _, n := range names {
		n = strings.TrimPrefix(n, "@")
		var users []struct {
			ID int `json:"id"`
		}
		if err := c.do(ctx, http.MethodGet, "/users?username="+url.QueryEscape(n), nil, &users); err != nil {
			res.Warnings = append(res.Warnings, "user "+n+": "+err.Error())
			continue
		}
		if len(users) == 0 {
			res.Warnings = append(res.Warnings, "user "+n+": not found")
			continue
		}
		ids = append(ids, users[0].ID)
	}
	return ids
}

func (g *gitLab) milestone(ctx context.Context, c *apiClient, project, m string) (int, error) {
	var all []struct {
		ID    int    `json:"id"`
		IID   int    `json:"iid"`
		Title string `json:"title"`
	}
	if err := c.do(ctx, http.MethodGet, project+"/milestones?state=active&per_page=100", nil, &all); err != nil {
		return 0, err
	}
	for _, x := range all {
		if strings.EqualFold(x.Title, m) || strconv.Itoa(x.IID) == m {
			return x.ID, nil
		}
	}
	return 0, fmt.Errorf("no active milestone %q", m)
}
