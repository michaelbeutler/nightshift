package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/michaelbeutler/nightshift/internal/engine"
	"github.com/michaelbeutler/nightshift/internal/git"
	"github.com/michaelbeutler/nightshift/internal/mcp"
	"github.com/michaelbeutler/nightshift/internal/queue"
	"github.com/michaelbeutler/nightshift/internal/service"
	"github.com/michaelbeutler/nightshift/internal/version"
)

const mcpInstructions = `nightshift is "schedule send" for git. Use it when the user wants work
delivered later instead of now (by default inside the nightly window):

- schedule_commit instead of git commit: it buffers the staged changes (plus any
  paths you pass) as a commit that is created, dated and signed at its scheduled
  time. Until then the changes stay staged in the working tree. Do NOT also
  commit them with git; do not run git commit, git stash or git reset on them.
- schedule_pull_request instead of git push + gh pr create / glab mr create: it
  schedules the push too. schedule_push alone only pushes.
- If commits were already made with plain git commit and not pushed,
  adopt_commits turns them into buffered commits.
- branch_status shows what is planned for a branch; list_jobs shows everything.

Times ("at"): an exact date and time is kept to the second, e.g.
"2026-12-01 02:00", "2026-12-01T02:00:00+01:00", "tomorrow 01:30", "+2h",
optionally with a zone ("Europe/Zurich"). Loose times let nightshift pick:
"tonight" (the default window), "now", "01:00-03:00", "2026-12-01". Always
pass "repo" as the absolute path of the repository you are working in.`

// mcpCmd serves the tools over stdio. Everything but protocol messages goes
// to stderr: hook output, progress, logs.
func (a *app) mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve nightshift to AI agents over MCP (stdio)",
		Long: `Run a Model Context Protocol server on stdin/stdout, so agents such as Claude
Code, Codex or Cursor can schedule commits, pushes and pull requests.

  claude mcp add nightshift -- nightshift mcp
  codex mcp add nightshift -- nightshift mcp`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			srv := &mcp.Server{
				Name:         "nightshift",
				Title:        "nightshift: schedule send for git",
				Version:      version.Short(),
				Instructions: mcpInstructions,
				Tools:        a.tools(eng),
			}
			return srv.Serve(cmd.Context(), a.in, a.out)
		},
	}
}

// decode unmarshals a tool's arguments.
func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("invalid arguments: %w", err)
	}
	return v, nil
}

func (a *app) repoArg(repo string) string {
	if repo != "" {
		return repo
	}
	return a.workdir()
}

func obj(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func strs(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

const (
	repoDesc = "Absolute path of the git repository (default: the server's working directory)"
	atDesc   = "When. Exact (kept to the second): 2026-12-01 02:00, 2026-12-01T02:00:00+01:00, 01.12.2026 02:00, dec 1 2am, tomorrow 01:30, fri 02:00, 02:30, +2h, @unix; a zone may follow (UTC, +01:00, Europe/Zurich). Loose (nightshift picks): tonight (default), now, 01:00-03:00, 2026-12-01, fri"
	idDesc   = "Job ID, or a unique prefix of it"
)

func (a *app) tools(eng *engine.Engine) []mcp.Tool {
	readOnly := &mcp.Annotations{ReadOnly: true, Idempotent: true}
	local := &mcp.Annotations{}
	return []mcp.Tool{
		{
			Name:  "schedule_commit",
			Title: "Buffer a commit for later",
			Description: "Buffer the staged changes as a commit that nightshift creates (dated and signed) at its scheduled time. " +
				"Use instead of git commit when the user wants the work delivered later. Pass paths to stage them first, or all to stage every modified tracked file.",
			InputSchema: obj([]string{"message"}, map[string]any{
				"repo": str(repoDesc), "message": str("The full commit message"),
				"paths": strs("Paths to stage before buffering (relative to repo)"), "all": boolean("Stage all modified and deleted tracked files first"),
				"amend": boolean("Replace the branch's last buffered commit"), "allow_empty": boolean("Allow a commit with no changes"),
				"no_verify": boolean("Skip the pre-commit and commit-msg hooks"), "signoff": boolean("Add a Signed-off-by trailer"),
				"at": str(atDesc),
			}),
			Annotations: local,
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[struct {
					Repo       string   `json:"repo"`
					Message    string   `json:"message"`
					Paths      []string `json:"paths"`
					All        bool     `json:"all"`
					Amend      bool     `json:"amend"`
					AllowEmpty bool     `json:"allow_empty"`
					NoVerify   bool     `json:"no_verify"`
					Signoff    bool     `json:"signoff"`
					At         string   `json:"at"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if in.Message == "" {
					return nil, errors.New("message is required")
				}
				return eng.Commit(ctx, engine.CommitOptions{
					Dir: a.repoArg(in.Repo), Message: in.Message, Paths: in.Paths, All: in.All, Amend: in.Amend,
					AllowEmpty: in.AllowEmpty, NoVerify: in.NoVerify, Signoff: in.Signoff, At: in.At,
				})
			},
		},
		{
			Name:        "adopt_commits",
			Title:       "Turn unpushed commits into buffered ones",
			Description: "Turn commits already made with plain git commit, and on no remote yet, into buffered commits that are recreated with night dates and fresh signatures. The branch moves back (like git reset --soft); all changes stay staged.",
			InputSchema: obj(nil, map[string]any{
				"repo": str(repoDesc), "since": str("Only adopt commits not reachable from this revision, e.g. origin/main"), "at": str("When the first one is created; " + atDesc),
			}),
			Annotations: local,
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[struct{ Repo, Since, At string }](raw)
				if err != nil {
					return nil, err
				}
				return eng.Adopt(ctx, engine.AdoptOptions{Dir: a.repoArg(in.Repo), Since: in.Since, At: in.At})
			},
		},
		{
			Name:        "schedule_push",
			Title:       "Schedule a push",
			Description: "Schedule a push of the branch (after its buffered commits are created). Use instead of git push when the user wants it delivered later.",
			InputSchema: obj(nil, map[string]any{
				"repo": str(repoDesc), "remote": str("Remote (default: the branch's push remote, else origin)"),
				"branch": str("Branch (default: the current one)"), "at": str(atDesc),
				"force_with_lease": boolean("Push with --force-with-lease"),
			}),
			Annotations: local,
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[struct {
					Repo, Remote, Branch, At string
					ForceWithLease           bool `json:"force_with_lease"`
				}](raw)
				if err != nil {
					return nil, err
				}
				return eng.Push(ctx, engine.PushOptions{Dir: a.repoArg(in.Repo), Remote: in.Remote, Branch: in.Branch, At: in.At, ForceWithLease: in.ForceWithLease})
			},
		},
		{
			Name:  "schedule_pull_request",
			Title: "Schedule a pull request or merge request",
			Description: "Schedule a GitHub pull request or GitLab merge request with title, Markdown body, reviewers, assignees, labels and milestone, " +
				"opened after the branch is pushed; the push is scheduled too if needed. Use instead of gh pr create / glab mr create when the user wants it delivered later.",
			InputSchema: obj(nil, map[string]any{
				"repo": str(repoDesc), "title": str("Title (required unless fill)"), "body": str("Body in Markdown"),
				"fill":  boolean("Take title and body from the commits when not given"),
				"base":  str("Branch to merge into (default: the repository's default branch)"),
				"draft": boolean("Open as a draft"), "reviewers": strs("Reviewers: usernames, or org/team on GitHub"),
				"assignees": strs("Assignees"), "labels": strs("Labels"), "milestone": str("Milestone title"),
				"remote": str("Remote the branch is pushed to"), "target_repo": str("owner/name to open it in, for a pull request from a fork"),
				"at": str(atDesc), "replace": boolean("Replace a pull request already scheduled for the branch"),
			}),
			Annotations: &mcp.Annotations{OpenWorld: true},
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[struct {
					Repo, Title, Body, Base, Milestone, Remote, At string
					TargetRepo                                     string `json:"target_repo"`
					Fill, Replace                                  bool
					Draft                                          *bool
					Reviewers, Assignees, Labels                   []string
				}](raw)
				if err != nil {
					return nil, err
				}
				return eng.CreatePR(ctx, engine.PROptions{
					Dir: a.repoArg(in.Repo), Title: in.Title, Body: in.Body, Fill: in.Fill, Base: in.Base, Draft: in.Draft,
					Reviewers: in.Reviewers, Assignees: in.Assignees, Labels: in.Labels, Milestone: in.Milestone,
					Remote: in.Remote, Repo: in.TargetRepo, At: in.At, Replace: in.Replace,
				})
			},
		},
		{
			Name:        "branch_status",
			Title:       "What is planned for a branch",
			Description: "Show the buffered commits, push and pull request planned for the repository's current branch, and staged changes not buffered yet.",
			InputSchema: obj(nil, map[string]any{"repo": str(repoDesc)}),
			Annotations: readOnly,
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[struct{ Repo string }](raw)
				if err != nil {
					return nil, err
				}
				return eng.Status(ctx, a.repoArg(in.Repo))
			},
		},
		{
			Name:        "list_jobs",
			Title:       "List scheduled jobs",
			Description: "List scheduled jobs (commits, pushes, pull requests), soonest first.",
			InputSchema: obj(nil, map[string]any{"repo": str("Only jobs of this repository (absolute path); default: all"), "all": boolean("Include done and canceled jobs")}),
			Annotations: readOnly,
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[struct {
					Repo string
					All  bool
				}](raw)
				if err != nil {
					return nil, err
				}
				f := engine.Filter{All: in.All}
				if in.Repo != "" {
					repo, err := git.Open(ctx, in.Repo)
					if err != nil {
						return nil, err
					}
					f.Repo = repo.Dir
				}
				jobs, err := eng.Jobs(ctx, f)
				return map[string]any{"jobs": nonNilJobs(jobs)}, err
			},
		},
		{
			Name:        "show_job",
			Title:       "Show a job",
			Description: "Show one job in full: its time, status, error, and for a pull request its body.",
			InputSchema: obj([]string{"id"}, map[string]any{"id": str(idDesc)}),
			Annotations: readOnly,
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[struct{ ID string }](raw)
				if err != nil {
					return nil, err
				}
				return eng.Job(ctx, in.ID)
			},
		},
		{
			Name:        "reschedule_job",
			Title:       "Move a job",
			Description: "Move a job to another time; everything that waits for it moves by the same amount. \"now\" ships it on the runner's next pass.",
			InputSchema: obj([]string{"id", "at"}, map[string]any{"id": str(idDesc), "at": str(atDesc)}),
			Annotations: local,
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[struct{ ID, At string }](raw)
				if err != nil {
					return nil, err
				}
				jobs, err := eng.Reschedule(ctx, in.ID, in.At)
				return map[string]any{"moved": nonNilJobs(jobs)}, err
			},
		},
		{
			Name:        "cancel_job",
			Title:       "Cancel a job",
			Description: "Cancel a job and what depends on it. Canceling a buffered commit cancels the ones after it; the changes stay in the working tree.",
			InputSchema: obj([]string{"id"}, map[string]any{"id": str(idDesc)}),
			Annotations: &mcp.Annotations{Destructive: true},
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[struct{ ID string }](raw)
				if err != nil {
					return nil, err
				}
				jobs, err := eng.Cancel(ctx, in.ID)
				return map[string]any{"canceled": nonNilJobs(jobs)}, err
			},
		},
		{
			Name:        "retry_job",
			Title:       "Retry a failed job",
			Description: "Queue a failed job again; it keeps its time (a commit keeps its date).",
			InputSchema: obj([]string{"id"}, map[string]any{"id": str(idDesc)}),
			Annotations: local,
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[struct{ ID string }](raw)
				if err != nil {
					return nil, err
				}
				return eng.Retry(ctx, in.ID)
			},
		},
		{
			Name:        "doctor",
			Title:       "Check the setup",
			Description: "Check that scheduled work can run unattended: git, the background runner, push access, signing and pull request authentication.",
			InputSchema: obj(nil, map[string]any{"repo": str(repoDesc)}),
			Annotations: readOnly,
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				in, err := decode[struct{ Repo string }](raw)
				if err != nil {
					return nil, err
				}
				return map[string]any{"checks": eng.Doctor(ctx, a.repoArg(in.Repo), service.Status)}, nil
			},
		},
	}
}

func nonNilJobs(jobs []*queue.Job) []*queue.Job {
	if jobs == nil {
		return []*queue.Job{}
	}
	return jobs
}
