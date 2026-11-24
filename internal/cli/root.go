// Package cli is nightshift's command line, built with Cobra and Viper.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/michaelbeutler/nightshift/internal/config"
	"github.com/michaelbeutler/nightshift/internal/engine"
	"github.com/michaelbeutler/nightshift/internal/notify"
	"github.com/michaelbeutler/nightshift/internal/queue"
	"github.com/michaelbeutler/nightshift/internal/schedule"
	"github.com/michaelbeutler/nightshift/internal/version"
)

type app struct {
	in     io.Reader
	out    io.Writer
	errOut io.Writer

	v       *viper.Viper
	cfgFile string
	dir     string
	jsonOut bool
	noColor bool

	eng *engine.Engine
	st  style
}

// Execute runs the nightshift command line and returns the exit code.
func Execute(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	a := &app{in: in, out: out, errOut: errOut, v: viper.New()}
	root := a.root()
	root.SetArgs(args)
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errOut)
	if err := root.ExecuteContext(ctx); err != nil {
		a.fail(err)
		return 1
	}
	return 0
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "nightshift",
		Short: "Schedule send for git: commit now, ship tonight",
		Long: `nightshift is "schedule send" for git.

Like scheduling an email or a Teams message to go out later, you prepare
your commits, the push and the pull request whenever you like, and nightshift
delivers them in the night window: the commits are created, dated and signed
at their scheduled time, then pushed, then the pull request or merge request
is opened, with its body, reviewers, labels and the rest.

It uses what is installed: your git (SSH keys, credential helpers, signing,
hooks), and gh or glab when they are there (else the REST API).`,
		Example: `  nightshift commit -am "feat: parse nested blocks"   # buffer a commit for tonight
  nightshift pr create --fill --reviewer octocat     # and the push and PR after it
  nightshift status                                   # what is planned for this branch
  nightshift service install                          # run the queue in the background`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.String(),
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			a.st = style{on: colorEnabled(a.out, a.noColor) && !a.jsonOut}
		},
	}
	root.SetVersionTemplate("nightshift {{.Version}}\n")
	pf := root.PersistentFlags()
	pf.StringVar(&a.cfgFile, "config", "", "config file (default $XDG_CONFIG_HOME/nightshift/config.yaml)")
	pf.StringVarP(&a.dir, "repo", "C", "", "run as if started in this directory")
	pf.BoolVar(&a.jsonOut, "json", false, "print machine-readable JSON (for scripts and agents)")
	pf.BoolVar(&a.noColor, "no-color", false, "disable colors (NO_COLOR is honored too)")

	root.AddGroup(
		&cobra.Group{ID: "schedule", Title: "Schedule work:"},
		&cobra.Group{ID: "plan", Title: "See and change the plan:"},
		&cobra.Group{ID: "run", Title: "Run the queue:"},
		&cobra.Group{ID: "setup", Title: "Set up:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("schedule", a.commitCmd(), a.adoptCmd(), a.pushCmd(), a.prCmd())
	add("plan", a.statusCmd(), a.listCmd(), a.showCmd(), a.rescheduleCmd(), a.cancelCmd(), a.retryCmd())
	add("run", a.runCmd(), a.daemonCmd(), a.serviceCmd())
	add("setup", a.doctorCmd(), a.configCmd(), a.mcpCmd(), a.versionCmd())
	root.SetHelpCommandGroupID("setup")
	root.SetCompletionCommandGroupID("setup")
	return root
}

// engine loads the configuration and opens the store on first use, so
// --help and version work with a broken config.
func (a *app) engine() (*engine.Engine, error) {
	if a.eng != nil {
		return a.eng, nil
	}
	cfg, err := config.Load(a.v, a.cfgFile)
	if err != nil {
		return nil, err
	}
	paths, err := config.DefaultPaths()
	if err != nil {
		return nil, err
	}
	store, err := queue.Open(paths.StateDir)
	if err != nil {
		return nil, err
	}
	eng, err := engine.New(store, cfg)
	if err != nil {
		return nil, err
	}
	eng.Log = a.errOut
	eng.Notify = notify.Send
	// NIGHTSHIFT_NOW pins the clock, for tests and for recording the demo.
	if fixed := os.Getenv("NIGHTSHIFT_NOW"); fixed != "" {
		if err := eng.PinClock(fixed); err != nil {
			return nil, fmt.Errorf("NIGHTSHIFT_NOW: %w", err)
		}
	}
	a.eng = eng
	return eng, nil
}

func (a *app) workdir() string {
	if a.dir != "" {
		if abs, err := filepath.Abs(a.dir); err == nil {
			return abs
		}
		return a.dir
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// emit prints v as JSON with --json, and calls human otherwise.
func (a *app) emit(v any, human func()) error {
	if a.jsonOut {
		enc := json.NewEncoder(a.out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	human()
	return nil
}

func (a *app) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.out, format, args...)
}

func (a *app) fail(err error) {
	if a.jsonOut {
		_ = json.NewEncoder(a.out).Encode(map[string]string{"error": err.Error()})
	}
	st := style{on: colorEnabled(a.errOut, a.noColor)}
	msg := err.Error()
	if hint := hintFor(err); hint != "" {
		msg += "\n" + st.dim("hint: "+hint)
	}
	_, _ = fmt.Fprintf(a.errOut, "%s %s\n", st.red("nightshift:"), msg)
}

func hintFor(err error) string {
	var moved *engine.BranchMovedError
	switch {
	case errors.As(err, &moved):
		return "`nightshift status` shows the buffered commits; `nightshift cancel <id>` drops them (the changes stay in your working tree)"
	case errors.As(err, new(*engine.TooEarlyError)):
		return "`nightshift status` shows when each step of the branch runs"
	case errors.Is(err, schedule.ErrPast):
		return "an exact time must be in the future; `now` runs it on the next pass"
	case errors.Is(err, engine.ErrEmptyMessage):
		return "pass -m \"message\" or -F file"
	case errors.Is(err, queue.ErrNotFound):
		return "`nightshift list --all` shows every job ID"
	case strings.Contains(err.Error(), "unknown git host"):
		return "see `nightshift config init` for the providers section"
	}
	return ""
}
