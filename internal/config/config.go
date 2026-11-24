// Package config loads nightshift's settings with Viper: built-in defaults,
// then the config file, then NIGHTSHIFT_* environment variables, then flags.
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/michaelbeutler/nightshift/internal/schedule"
)

// Template is the commented config file `nightshift config init` writes.
//
//go:embed template.yaml
var Template string

// Config is the effective configuration.
type Config struct {
	// Remote is the remote to push to when the branch names none.
	Remote string `mapstructure:"remote" json:"remote"`
	// Sign is "auto" (follow commit.gpgSign), "always" or "never".
	Sign string `mapstructure:"sign" json:"sign"`
	// Notify is "failures", "all" or "off".
	Notify    string   `mapstructure:"notify" json:"notify"`
	Schedule  Schedule `mapstructure:"schedule" json:"schedule"`
	PR        PR       `mapstructure:"pr" json:"pr"`
	Runner    Runner   `mapstructure:"runner" json:"runner"`
	Providers []Host   `mapstructure:"providers" json:"providers,omitempty"`
}

// Schedule decides when things happen if no time is given.
type Schedule struct {
	// Window is the nightly range new work is scheduled into, "01:00-04:00".
	Window string `mapstructure:"window" json:"window"`
	// Spacing is the random gap between consecutive steps, "3m-15m".
	Spacing string `mapstructure:"spacing" json:"spacing"`
	// Missed is what happens to a job the machine slept through: "run" it
	// on wake, or move it to the "next-window".
	Missed string `mapstructure:"missed" json:"missed"`
	// Grace is how late a job may run before Missed applies.
	Grace time.Duration `mapstructure:"grace" json:"grace"`
}

// PR holds pull request defaults.
type PR struct {
	// Method is "auto" (gh/glab when installed and logged in, else the
	// REST API), "cli" or "api".
	Method    string   `mapstructure:"method" json:"method"`
	Base      string   `mapstructure:"base" json:"base,omitempty"`
	Draft     bool     `mapstructure:"draft" json:"draft"`
	Reviewers []string `mapstructure:"reviewers" json:"reviewers,omitempty"`
	Assignees []string `mapstructure:"assignees" json:"assignees,omitempty"`
	Labels    []string `mapstructure:"labels" json:"labels,omitempty"`
}

// Runner tunes `nightshift run` and `nightshift daemon`.
type Runner struct {
	Interval time.Duration `mapstructure:"interval" json:"interval"`
	Retries  int           `mapstructure:"retries" json:"retries"`
}

// Host tells nightshift what a self-hosted or aliased git host is.
type Host struct {
	Host string `mapstructure:"host" json:"host"`
	// Type is "github" or "gitlab".
	Type string `mapstructure:"type" json:"type"`
	// API overrides the REST API base URL.
	API string `mapstructure:"api" json:"api,omitempty"`
	// TokenEnv names an environment variable that holds the API token.
	TokenEnv string `mapstructure:"token_env" json:"token_env,omitempty"`
	// TokenCmd is a command that prints the API token, such as
	// "op read op://dev/gitlab/token" or "security find-generic-password -w ...".
	TokenCmd string `mapstructure:"token_cmd" json:"token_cmd,omitempty"`
}

// SetDefaults registers every key with its default, which is also what lets
// NIGHTSHIFT_* environment variables override any of them.
func SetDefaults(v *viper.Viper) {
	v.SetDefault("remote", "origin")
	v.SetDefault("sign", "auto")
	v.SetDefault("notify", "failures")
	v.SetDefault("schedule.window", "01:00-04:00")
	v.SetDefault("schedule.spacing", "3m-15m")
	v.SetDefault("schedule.missed", "run")
	v.SetDefault("schedule.grace", "2h")
	v.SetDefault("pr.method", "auto")
	v.SetDefault("pr.base", "")
	v.SetDefault("pr.draft", false)
	v.SetDefault("pr.reviewers", []string{})
	v.SetDefault("pr.assignees", []string{})
	v.SetDefault("pr.labels", []string{})
	v.SetDefault("runner.interval", "30s")
	v.SetDefault("runner.retries", 3)
	v.SetDefault("providers", []map[string]any{})
}

// Default returns the built-in configuration, without reading any file or
// environment variable.
func Default() *Config {
	v := viper.New()
	SetDefaults(v)
	var c Config
	if err := v.Unmarshal(&c); err != nil {
		panic("config: the defaults do not decode: " + err.Error())
	}
	return &c
}

// Paths are where nightshift keeps its files.
type Paths struct {
	ConfigFile string `json:"config_file"`
	StateDir   string `json:"state_dir"`
	LogFile    string `json:"log_file"`
}

// DefaultPaths follows the XDG base directory layout on every Unix (macOS
// included, as most CLIs do), and %AppData%/%LocalAppData% on Windows.
// NIGHTSHIFT_HOME puts everything under one directory instead.
func DefaultPaths() (Paths, error) {
	if home := os.Getenv("NIGHTSHIFT_HOME"); home != "" {
		return Paths{
			ConfigFile: filepath.Join(home, "config.yaml"),
			StateDir:   filepath.Join(home, "state"),
			LogFile:    filepath.Join(home, "state", "nightshift.log"),
		}, nil
	}
	var cfgDir, stateDir string
	if runtime.GOOS == "windows" {
		c, err := os.UserConfigDir()
		if err != nil {
			return Paths{}, err
		}
		cfgDir = filepath.Join(c, "nightshift")
		local := os.Getenv("LocalAppData")
		if local == "" {
			local = c
		}
		stateDir = filepath.Join(local, "nightshift")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, err
		}
		cfgDir = xdg("XDG_CONFIG_HOME", filepath.Join(home, ".config"), "nightshift")
		stateDir = xdg("XDG_STATE_HOME", filepath.Join(home, ".local", "state"), "nightshift")
	}
	return Paths{
		ConfigFile: filepath.Join(cfgDir, "config.yaml"),
		StateDir:   stateDir,
		LogFile:    filepath.Join(stateDir, "nightshift.log"),
	}, nil
}

func xdg(env, fallback, app string) string {
	if d := os.Getenv(env); filepath.IsAbs(d) {
		return filepath.Join(d, app)
	}
	return filepath.Join(fallback, app)
}

// Load reads the config file (file, or the default path when empty; a
// missing default file is fine) and returns the validated configuration.
func Load(v *viper.Viper, file string) (*Config, error) {
	SetDefaults(v)
	v.SetEnvPrefix("NIGHTSHIFT")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	v.SetConfigType("yaml")
	if file != "" {
		v.SetConfigFile(file)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config %s: %w", file, err)
		}
	} else {
		p, err := DefaultPaths()
		if err != nil {
			return nil, err
		}
		v.SetConfigFile(p.ConfigFile)
		if err := v.ReadInConfig(); err != nil && !errors.Is(err, os.ErrNotExist) {
			var nf viper.ConfigFileNotFoundError
			if !errors.As(err, &nf) {
				return nil, fmt.Errorf("read config %s: %w", p.ConfigFile, err)
			}
		}
	}
	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks every enumerated and parsed value, so a typo in the config
// fails at once rather than at two in the morning.
func (c *Config) Validate() error {
	var errs []error
	if _, err := schedule.ParseWindow(c.Schedule.Window); err != nil {
		errs = append(errs, fmt.Errorf("schedule.window: %w", err))
	}
	if _, err := schedule.ParseSpacing(c.Schedule.Spacing); err != nil {
		errs = append(errs, fmt.Errorf("schedule.spacing: %w", err))
	}
	errs = append(errs,
		oneOf("schedule.missed", c.Schedule.Missed, "run", "next-window"),
		oneOf("sign", c.Sign, "auto", "always", "never"),
		oneOf("notify", c.Notify, "failures", "all", "off"),
		oneOf("pr.method", c.PR.Method, "auto", "cli", "api"),
	)
	if c.Runner.Interval < time.Second {
		errs = append(errs, fmt.Errorf("runner.interval %s: must be at least 1s", c.Runner.Interval))
	}
	for i, h := range c.Providers {
		if h.Host == "" {
			errs = append(errs, fmt.Errorf("providers[%d]: host is required", i))
		}
		errs = append(errs, oneOf(fmt.Sprintf("providers[%d].type", i), h.Type, "github", "gitlab"))
	}
	return errors.Join(errs...)
}

func oneOf(key, val string, allowed ...string) error {
	for _, a := range allowed {
		if val == a {
			return nil
		}
	}
	return fmt.Errorf("%s %q: want one of %s", key, val, strings.Join(allowed, ", "))
}
