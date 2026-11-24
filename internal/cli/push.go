package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/michaelbeutler/nightshift/internal/engine"
	"github.com/michaelbeutler/nightshift/internal/git"
)

func (a *app) pushCmd() *cobra.Command {
	var o engine.PushOptions
	cmd := &cobra.Command{
		Use:   "push [<remote> [<branch>]]",
		Short: "Schedule a push of the branch",
		Long: `Schedule a push of the branch to its remote. With buffered commits, the push
runs a few minutes after the last of them has been created and carries all of
them; commits buffered later move it further back. Without buffered commits it
pushes the branch as it is now.

The push is a plain git push run by your git: SSH keys, ssh-agent, credential
helpers, insteadOf rewrites and pre-push hooks all apply.`,
		Example: `  nightshift push
  nightshift push origin feature/login --at "tomorrow 06:30"`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			o.Dir = a.workdir()
			if len(args) > 0 {
				o.Remote = args[0]
			}
			if len(args) > 1 {
				o.Branch = args[1]
			}
			res, err := eng.Push(cmd.Context(), o)
			if err != nil {
				return err
			}
			return a.emit(res, func() {
				verb := "Scheduled"
				if res.Rescheduled {
					verb = "Rescheduled"
				}
				a.printf("%s %s the push of %s to %s for %s\n", a.st.blue("↑"), verb, a.st.cyan(res.Job.Branch), res.Job.Remote, a.when(res.Job.At))
				if res.Commits > 0 {
					a.printf("    %s\n", a.st.dim(plural(res.Commits, "buffered commit")+", created before it"))
				}
				a.printMoved(res.Moved)
			})
		},
	}
	cmd.Flags().StringVar(&o.At, "at", "", atHelp)
	cmd.Flags().BoolVar(&o.ForceWithLease, "force-with-lease", false, "push with --force-with-lease")
	return cmd
}

func (a *app) prCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "pr",
		Aliases: []string{"mr"},
		Short:   "Schedule pull requests (merge requests on GitLab)",
	}
	cmd.AddCommand(a.prCreateCmd())
	return cmd
}

func (a *app) prCreateCmd() *cobra.Command {
	var (
		o        engine.PROptions
		bodyFile string
		draft    bool
	)
	cmd := &cobra.Command{
		Use:     "create",
		Aliases: []string{"new"},
		Short:   "Schedule a pull request, and the push it needs",
		Long: `Schedule a pull request on GitHub or a merge request on GitLab, opened a few
minutes after the branch is pushed. If the branch has buffered commits or is
not on the remote yet, the push is scheduled too.

It is opened with gh or glab when they are installed and logged in, and with
the REST API otherwise (token from token_cmd/token_env, GH_TOKEN/GITLAB_TOKEN,
the CLI's stored login, or your git credential helper).

Without --body, --body-file or --fill, your editor opens on the repository's
pull request template when you are at a terminal.`,
		Example: `  nightshift pr create --fill
  nightshift pr create -t "feat: parse nested blocks" -F pr.md -r octocat -r acme/core -l enhancement
  nightshift mr create --title "Fix login" --body "Closes #12" --draft --at 07:45`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			o.Dir = a.workdir()
			if cmd.Flags().Changed("draft") {
				o.Draft = &draft
			}
			if bodyFile != "" {
				data, err := readInput(bodyFile, a.in)
				if err != nil {
					return err
				}
				o.Body = string(data)
			}
			if o.Body == "" && !o.Fill && !cmd.Flags().Changed("body") && !a.jsonOut && isTerminal(a.in) && isTerminal(a.errOut) {
				if o.Body, err = a.editBody(cmd.Context()); err != nil {
					return err
				}
			}
			res, err := eng.CreatePR(cmd.Context(), o)
			if err != nil {
				return err
			}
			return a.emit(res, func() {
				j := res.Job
				target := j.PR.Base
				if target == "" {
					target = "the default branch"
				}
				a.printf("%s Scheduled a pull request %s → %s on %s for %s\n", a.st.cyan("⇄"), a.st.cyan(j.Branch), target, res.Host, a.when(j.At))
				a.printf("    %s\n", j.PR.Title)
				if m := meta(j.PR); m != "" {
					a.printf("    %s\n", a.st.dim(m))
				}
				if res.Push != nil {
					a.printf("    %s %s %s\n", a.st.dim("after"), a.kind(res.Push), a.st.dim(res.Push.ID)+" at "+when(res.Push.At, eng.Now()))
				}
				if res.Warning != "" {
					a.printf("    %s %s\n", a.st.yellow("!"), res.Warning)
				}
			})
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.Title, "title", "t", "", "the title")
	f.StringVarP(&o.Body, "body", "b", "", "the body (Markdown)")
	f.StringVarP(&bodyFile, "body-file", "F", "", "read the body from a file (- for stdin)")
	f.BoolVar(&o.Fill, "fill", false, "take the title and body from the commits")
	f.StringVarP(&o.Base, "base", "B", "", "the branch to merge into (default: the repository's default branch)")
	f.BoolVarP(&draft, "draft", "d", false, "open it as a draft")
	f.StringSliceVarP(&o.Reviewers, "reviewer", "r", nil, "request a review (user, or org/team on GitHub); repeatable")
	f.StringSliceVarP(&o.Assignees, "assignee", "a", nil, "assign someone; repeatable")
	f.StringSliceVarP(&o.Labels, "label", "l", nil, "add a label; repeatable")
	f.StringVarP(&o.Milestone, "milestone", "m", "", "the milestone, by title")
	f.StringVar(&o.Remote, "remote", "", "the remote the branch is pushed to")
	f.StringVar(&o.Repo, "target-repo", "", "open it in another repository (owner/name), as from a fork")
	f.StringVar(&o.At, "at", "", atHelp)
	f.BoolVar(&o.Replace, "replace", false, "replace the pull request already scheduled for this branch")
	cmd.MarkFlagsMutuallyExclusive("body", "body-file")
	return cmd
}

// prTemplates are where GitHub and GitLab look for a description template.
var prTemplates = []string{
	".github/pull_request_template.md", ".github/PULL_REQUEST_TEMPLATE.md",
	"pull_request_template.md", "PULL_REQUEST_TEMPLATE.md",
	"docs/pull_request_template.md", "docs/PULL_REQUEST_TEMPLATE.md",
	".gitlab/merge_request_templates/Default.md", ".gitlab/merge_request_templates/default.md",
}

func (a *app) editBody(ctx context.Context) (string, error) {
	repo, err := git.Open(ctx, a.workdir())
	if err != nil {
		return "", err
	}
	gitDir, err := repo.GitDir(ctx)
	if err != nil {
		return "", err
	}
	content := ""
	for _, t := range prTemplates {
		if data, err := os.ReadFile(filepath.Join(repo.Dir, t)); err == nil {
			content = string(data)
			break
		}
	}
	return a.edit(ctx, repo, filepath.Join(gitDir, "NIGHTSHIFT_PR_BODY.md"), content)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
