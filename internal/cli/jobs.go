package cli

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/michaelbeutler/nightshift/internal/engine"
	"github.com/michaelbeutler/nightshift/internal/git"
	"github.com/michaelbeutler/nightshift/internal/queue"
)

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what is planned for the current branch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			s, err := eng.Status(cmd.Context(), a.workdir())
			if err != nil {
				return err
			}
			return a.emit(s, func() { a.printStatus(s) })
		},
	}
}

func (a *app) printStatus(s *engine.Status) {
	a.printf("On branch %s\n", a.st.cyan(s.Branch))
	if s.Moved != "" {
		a.printf("  %s %s\n", a.st.red("✗"), s.Moved)
	}
	if len(s.Commits) == 0 && s.Push == nil && s.PR == nil {
		a.printf("  %s\n", a.st.dim("nothing scheduled; nightshift commit buffers the staged changes"))
	}
	line := func(icon, kind string, j *queue.Job, what string) {
		a.printf("  %s %-6s %s %s  %s\n", icon, kind, a.st.dim(j.ID), a.when(j.At), strings.TrimSpace(what+" "+a.flag(j)))
	}
	for _, j := range s.Commits {
		line(a.st.magenta("☾"), "commit", j, j.Title())
	}
	if j := s.Push; j != nil {
		line(a.st.blue("↑"), "push", j, "→ "+j.Remote)
	}
	if j := s.PR; j != nil {
		line(a.st.cyan("⇄"), "pr", j, a.details(j))
		if m := meta(j.PR); m != "" {
			a.printf("    %s\n", a.st.dim(m))
		}
	}
	if n := len(s.Unbuffered); n > 0 {
		a.printf("\n%s staged change(s) not buffered yet (nightshift commit buffers them):\n", strconv.Itoa(n))
		for _, l := range s.Unbuffered {
			a.printf("    %s\n", a.st.green(nameStatus(l)))
		}
	}
	if s.Elsewhere > 0 {
		a.printf("\n%s\n", a.st.dim(plural(s.Elsewhere, "more job")+" on other branches of this repository (nightshift list)"))
	}
}

// flag marks a job that is not simply waiting.
func (a *app) flag(j *queue.Job) string {
	switch {
	case j.Status == queue.StatusFailed:
		return a.st.red("failed: " + j.Error)
	case j.Status == queue.StatusRunning:
		return a.st.cyan("running")
	case !j.RetryAt.IsZero():
		return a.st.yellow("retrying at " + when(j.RetryAt, a.eng.Now()) + ": " + j.Error)
	case a.eng.Blocked(j):
		return a.st.yellow("blocked")
	}
	return ""
}

func (a *app) listCmd() *cobra.Command {
	var all, here bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls", "queue"},
		Short:   "List scheduled jobs in every repository",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			f := engine.Filter{All: all}
			if here {
				repo, err := git.Open(cmd.Context(), a.workdir())
				if err != nil {
					return err
				}
				f.Repo = repo.Dir
			}
			jobs, err := eng.Jobs(cmd.Context(), f)
			if err != nil {
				return err
			}
			if jobs == nil {
				jobs = []*queue.Job{}
			}
			return a.emit(jobs, func() {
				if len(jobs) == 0 {
					a.printf("%s\n", a.st.dim("Nothing scheduled. Buffer a commit with nightshift commit."))
					return
				}
				repos := map[string]bool{}
				for _, j := range jobs {
					repos[j.Repo] = true
				}
				a.table([]string{"ID", "KIND", "WHEN", "STATE", "BRANCH", "WHAT"}, a.jobRows(jobs, len(repos) > 1))
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include done and canceled jobs")
	cmd.Flags().BoolVar(&here, "here", false, "only jobs of the current repository")
	return cmd
}

func (a *app) showCmd() *cobra.Command {
	var patch bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a job; for a buffered commit, what it changes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			j, err := eng.Job(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.emit(j, func() {
				a.printf("%s %s  %s\n", a.kind(j), a.st.bold(j.ID), a.state(j))
				a.printf("%-10s%s  %s\n", "when", a.when(j.At), a.st.dim(j.At.Local().Format("2006-01-02 15:04:05 MST")))
				a.printf("%-10s%s\n", "repo", j.Repo)
				a.printf("%-10s%s\n", "branch", j.Branch)
				if j.Remote != "" {
					a.printf("%-10s%s\n", "remote", j.Remote)
				}
				if len(j.DependsOn) > 0 {
					a.printf("%-10s%s\n", "after", strings.Join(j.DependsOn, ", "))
				}
				if j.Error != "" {
					a.printf("%-10s%s\n", "error", a.st.red(j.Error))
				}
				if r := j.Result; r != nil {
					a.printf("%-10s%s\n", "result", strings.TrimSpace(strings.Join([]string{short(r.Commit), r.URL, r.Note}, " ")))
				}
				switch {
				case j.Commit != nil:
					a.printf("%-10s%s\n\n", "author", j.Commit.Author)
					for _, l := range strings.Split(strings.TrimRight(j.Commit.Message, "\n"), "\n") {
						a.printf("    %s\n", l)
					}
					if j.Active() {
						repo := &git.Repo{Dir: j.Repo}
						args := []string{"show", "--format=", "--stat", "--summary", j.Commit.Snapshot}
						if patch {
							args = []string{"show", "--format=", "--stat", "--patch", j.Commit.Snapshot}
						}
						if a.st.on {
							args = append(args[:1], append([]string{"--color=always"}, args[1:]...)...)
						}
						if out, err := repo.Run(cmd.Context(), args...); err == nil && out != "" {
							a.printf("\n%s\n", out)
						}
					}
				case j.PR != nil:
					a.printf("%-10s%s\n", "title", j.PR.Title)
					if m := meta(j.PR); m != "" {
						a.printf("%-10s%s\n", "", m)
					}
					if j.PR.Body != "" {
						a.printf("\n%s\n", j.PR.Body)
					}
				}
			})
		},
	}
	cmd.Flags().BoolVarP(&patch, "patch", "p", false, "show the full diff of a buffered commit")
	return cmd
}

func (a *app) rescheduleCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "reschedule <id> <when>",
		Aliases: []string{"move"},
		Short:   "Move a job, and everything after it, to another time",
		Long: `Move a job to another time. Everything that waits for it moves by the same
amount, so rescheduling a branch's first buffered commit moves the whole
branch; "now" ships it on the runner's next pass.

` + atHelp,
		Example: `  nightshift reschedule k3f9a2 "tomorrow 02:00"
  nightshift reschedule k3f9a2 now`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			moved, err := eng.Reschedule(cmd.Context(), args[0], strings.Join(args[1:], " "))
			if err != nil {
				return err
			}
			return a.emit(moved, func() {
				for _, j := range moved {
					a.printf("%s %s %s  %s\n", a.kind(j), a.st.dim(j.ID), a.when(j.At), j.Title())
				}
			})
		},
	}
}

func (a *app) cancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "cancel <id>...",
		Aliases: []string{"rm"},
		Short:   "Cancel jobs (your changes stay in the working tree)",
		Long: `Cancel jobs. Canceling a buffered commit also cancels the ones after it on the
branch, since their snapshots include its changes; the push then carries the
commits before it, or is canceled with its pull request when none is left.
Nothing is ever removed from your working tree or index.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			var all []*queue.Job
			for _, id := range args {
				jobs, err := eng.Cancel(cmd.Context(), id)
				if err != nil {
					return err
				}
				all = append(all, jobs...)
			}
			return a.emit(all, func() {
				for _, j := range all {
					a.printf("%s canceled %s %s  %s\n", a.st.red("✗"), a.kind(j), a.st.dim(j.ID), j.Title())
				}
			})
		},
	}
}

func (a *app) retryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry <id>",
		Short: "Queue a failed job again (it keeps its time and date)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			j, err := eng.Retry(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.emit(j, func() {
				a.printf("%s %s %s queued again; it runs on the runner's next pass\n", a.st.green("↻"), a.kind(j), a.st.dim(j.ID))
			})
		},
	}
}
