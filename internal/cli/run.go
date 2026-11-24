package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/michaelbeutler/nightshift/internal/config"
	"github.com/michaelbeutler/nightshift/internal/engine"
	"github.com/michaelbeutler/nightshift/internal/queue"
	"github.com/michaelbeutler/nightshift/internal/service"
)

func (a *app) runCmd() *cobra.Command {
	var o engine.RunOptions
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run every job that is due, then exit",
		Long: `Run every job whose time has come: create the buffered commits, push, open the
pull requests. Jobs the machine slept through run now (their commits keep
their night dates) unless schedule.missed is next-window.

The background service runs this every minute; run it yourself from cron or
to catch up by hand. Only one runner works at a time.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			// At a terminal the summary below says it all; under the service,
			// stdout is the log file and the timestamped lines are the record.
			interactive := isTerminal(a.out) || a.jsonOut
			if interactive {
				eng.Log = io.Discard
			}
			rep, err := eng.Run(cmd.Context(), o)
			if err != nil {
				return err
			}
			if !interactive && !o.DryRun {
				return nil
			}
			return a.emit(rep, func() {
				switch {
				case rep.Busy:
					a.printf("%s\n", a.st.dim("Another nightshift runner is working; nothing to do here."))
				case o.DryRun:
					if len(rep.Due) == 0 {
						a.printf("%s\n", a.st.dim("Nothing is due."))
					}
					for _, j := range rep.Due {
						a.printf("due %s %s  %s\n", a.kind(j), a.st.dim(j.ID), j.Title())
					}
				default:
					a.printRun(rep)
				}
			})
		},
	}
	cmd.Flags().BoolVar(&o.DryRun, "dry-run", false, "only list what is due")
	return cmd
}

func (a *app) printRun(rep *engine.RunReport) {
	if len(rep.Ran) == 0 {
		a.printf("%s\n", a.st.dim("Nothing was due."))
	}
	for _, o := range rep.Ran {
		mark, text := a.st.green("✓"), o.Job.Title()
		if r := o.Job.Result; r != nil && o.Error == "" {
			switch {
			case r.URL != "":
				text = r.URL
			case o.Job.Kind == queue.KindPush:
				text = short(r.Commit) + " → " + o.Job.Remote + "/" + o.Job.Branch
			case r.Commit != "":
				text = short(r.Commit) + " " + text
			}
			if r.Note != "" {
				text += " " + a.st.dim("("+r.Note+")")
			}
		}
		if o.Error != "" {
			mark, text = a.st.red("✗"), text+": "+a.st.red(o.Error)
		}
		a.printf("%s %s %s  %s\n", mark, a.paddedKind(o.Job), a.st.dim(o.Job.ID), text)
	}
	if rep.Next != nil {
		a.printf("%s %s %s %s\n", a.st.dim("next:"), a.kind(rep.Next), a.st.dim(rep.Next.ID), a.when(rep.Next.At))
	}
}

func (a *app) daemonCmd() *cobra.Command {
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Keep running due jobs until stopped (in the foreground)",
		Long: `Run due jobs every runner.interval until interrupted. Useful in a container, a
tmux session or under a process manager; on a laptop, nightshift service install
is the better fit. It compares wall-clock time on every tick, so it catches up
after sleep.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			if interval <= 0 {
				interval = eng.Config.Runner.Interval
			}
			ctx := cmd.Context()
			_, _ = fmt.Fprintf(a.errOut, "nightshift daemon: checking every %s (Ctrl-C to stop)\n", interval)
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for {
				if _, err := eng.Run(ctx, engine.RunOptions{}); err != nil && !errors.Is(err, context.Canceled) {
					_, _ = fmt.Fprintf(a.errOut, "nightshift daemon: %v\n", err)
				}
				select {
				case <-ctx.Done():
					return nil
				case <-tick.C:
				}
			}
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 0, "how often to check (default runner.interval)")
	return cmd
}

func (a *app) serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install the background runner (launchd, systemd, Task Scheduler)",
		Long: `Install, remove or inspect the background runner: a launchd agent on macOS, a
systemd user timer on Linux, a scheduled task on Windows. Each runs
nightshift run every minute and again after the machine wakes up.

The service gets your current PATH (so it finds git, gh, glab, gpg and ssh)
and your SSH agent socket where that is stable across reboots.`,
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install and start the background runner",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}
			paths, err := config.DefaultPaths()
			if err != nil {
				return err
			}
			files, err := service.Install(cmd.Context(), service.Spec{Executable: exe, Env: service.CaptureEnv(os.Getenv), LogFile: paths.LogFile})
			if err != nil {
				return err
			}
			return a.emit(map[string]any{"installed": files, "log": paths.LogFile}, func() {
				a.printf("%s Installed the background runner:\n", a.st.green("✓"))
				for _, f := range files {
					a.printf("    %s\n", f)
				}
				a.printf("    %s %s\n", a.st.dim("log:"), paths.LogFile)
				a.printf("%s\n", a.st.dim("It runs nightshift run every minute. Run nightshift doctor to check signing and push access."))
			})
		},
	}
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the background runner",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := service.Uninstall(cmd.Context()); err != nil {
				return err
			}
			return a.emit(map[string]bool{"uninstalled": true}, func() {
				a.printf("%s Removed the background runner. Scheduled jobs stay queued.\n", a.st.green("✓"))
			})
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Say whether the background runner is installed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ok, detail := service.Status(cmd.Context())
			return a.emit(map[string]any{"installed": ok, "detail": detail}, func() {
				mark := a.st.green("✓")
				if !ok {
					mark = a.st.yellow("!")
				}
				a.printf("%s %s\n", mark, detail)
			})
		},
	}
	cmd.AddCommand(install, uninstall, status)
	return cmd
}
