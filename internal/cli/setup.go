package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/michaelbeutler/nightshift/internal/config"
	"github.com/michaelbeutler/nightshift/internal/engine"
	"github.com/michaelbeutler/nightshift/internal/service"
	"github.com/michaelbeutler/nightshift/internal/version"
)

func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that tonight's run will work unattended",
		Long: `Check what the night run needs, each the way the runner does it (without a
terminal to prompt on): git, the background runner, your identity, push access
to the remote, signing, and the login or token for pull requests.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			checks := eng.Doctor(cmd.Context(), a.workdir(), service.Status)
			failed := false
			for _, c := range checks {
				failed = failed || c.Level == engine.LevelFail
			}
			err = a.emit(checks, func() {
				for _, c := range checks {
					a.printf("%s %-14s %s\n", checkMark(a.st, c.Level), c.Name, c.Detail)
					if c.Hint != "" {
						a.printf("  %-14s %s\n", "", a.st.dim(c.Hint))
					}
				}
			})
			if err == nil && failed {
				err = errors.New("some checks failed")
			}
			return err
		},
	}
}

func (a *app) configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show where nightshift keeps its files, and its settings",
	}
	var force bool
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Write a commented config file with every setting",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path := a.cfgFile
			if path == "" {
				p, err := config.DefaultPaths()
				if err != nil {
					return err
				}
				path = p.ConfigFile
			}
			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s exists; pass --force to overwrite it", path)
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(config.Template), 0o600); err != nil {
				return err
			}
			return a.emit(map[string]string{"config_file": path}, func() {
				a.printf("%s Wrote %s\n", a.st.green("✓"), path)
			})
		},
	}
	initCmd.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	path := &cobra.Command{
		Use:   "path",
		Short: "Print the config file, state directory and log file paths",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			p, err := config.DefaultPaths()
			if err != nil {
				return err
			}
			if a.cfgFile != "" {
				p.ConfigFile = a.cfgFile
			}
			return a.emit(p, func() {
				a.printf("config  %s\nstate   %s\nlog     %s\n", p.ConfigFile, p.StateDir, p.LogFile)
			})
		},
	}
	show := &cobra.Command{
		Use:   "show",
		Short: "Print the effective settings (defaults, file, environment)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			return a.emit(eng.Config, func() {
				out, err := yaml.Marshal(a.v.AllSettings())
				if err != nil {
					a.printf("%v\n", err)
					return
				}
				a.printf("%s", out)
			})
		},
	}
	cmd.AddCommand(initCmd, path, show)
	return cmd
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return a.emit(map[string]string{"version": version.String()}, func() {
				a.printf("nightshift %s\n", version.String())
			})
		},
	}
}
