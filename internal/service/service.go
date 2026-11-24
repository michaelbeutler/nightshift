// Package service installs the background runner with the operating
// system's own scheduler, which runs `nightshift run` every minute: a
// launchd agent on macOS, a systemd user timer on Linux, a scheduled task
// on Windows. Each wakes up after sleep, so missed jobs catch up.
package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Label names the service everywhere: the launchd label, the systemd
// unit and the scheduled task.
const Label = "io.github.michaelbeutler.nightshift"

// ErrUnsupported is returned where no scheduler is supported; the error
// says what to add to cron instead.
var ErrUnsupported = errors.New("no supported service manager on this system")

// Spec is what the service runs.
type Spec struct {
	// Executable is the absolute path of the nightshift binary.
	Executable string
	// Env is the environment it runs with: PATH (so git, gh, glab, gpg and
	// ssh are found), and the agent sockets that let it sign and push.
	Env map[string]string
	// LogFile receives the runner's output.
	LogFile string
}

// CaptureEnv picks the variables the runner needs from the current
// environment. macOS's own per-session SSH_AUTH_SOCK is left out: launchd
// gives agents the current one, while a saved path breaks after a reboot.
func CaptureEnv(getenv func(string) string) map[string]string {
	env := map[string]string{}
	for _, k := range []string{"PATH", "SSH_AUTH_SOCK", "GNUPGHOME", "NIGHTSHIFT_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "GH_HOST", "GITLAB_HOST"} {
		v := getenv(k)
		if v == "" || (k == "SSH_AUTH_SOCK" && strings.Contains(v, "com.apple.launchd")) {
			continue
		}
		env[k] = v
	}
	return env
}

// Files are the files Install writes, by path.
type Files map[string]string

// Render returns the files the service consists of on goos.
func Render(goos string, s Spec, home string) (Files, error) {
	switch goos {
	case "darwin":
		return Files{filepath.Join(home, "Library", "LaunchAgents", Label+".plist"): launchdPlist(s)}, nil
	case "linux":
		dir := filepath.Join(home, ".config", "systemd", "user")
		return Files{
			filepath.Join(dir, "nightshift.service"): systemdService(s),
			filepath.Join(dir, "nightshift.timer"):   systemdTimer,
		}, nil
	case "windows":
		return Files{}, nil
	}
	return nil, fmt.Errorf("%w; add this line to crontab -e instead:\n  * * * * * %s run >>%s 2>&1", ErrUnsupported, shellQuote(s.Executable), shellQuote(s.LogFile))
}

// Install writes and starts the service.
func Install(ctx context.Context, s Spec) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	files, err := Render(runtime.GOOS, s, home)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(s.LogFile), 0o700); err != nil {
		return nil, err
	}
	var paths []string
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // G306: service definitions hold no secrets and launchd wants them world-readable
			return nil, err
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	switch runtime.GOOS {
	case "darwin":
		domain := "gui/" + strconv.Itoa(os.Getuid())
		_ = run(ctx, "launchctl", "bootout", domain+"/"+Label)
		if err := run(ctx, "launchctl", "bootstrap", domain, paths[0]); err != nil {
			return paths, fmt.Errorf("launchctl bootstrap: %w", err)
		}
	case "linux":
		if err := run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return paths, err
		}
		if err := run(ctx, "systemctl", "--user", "enable", "--now", "nightshift.timer"); err != nil {
			return paths, err
		}
	case "windows":
		tr := `"` + s.Executable + `" run`
		if err := run(ctx, "schtasks", "/Create", "/F", "/SC", "MINUTE", "/MO", "1", "/TN", "nightshift", "/TR", tr); err != nil {
			return nil, err
		}
		paths = append(paths, `Task Scheduler\nightshift`)
	}
	return paths, nil
}

// Uninstall stops and removes the service.
func Uninstall(ctx context.Context) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	files, err := Render(runtime.GOOS, Spec{}, home)
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		_ = run(ctx, "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+Label)
	case "linux":
		_ = run(ctx, "systemctl", "--user", "disable", "--now", "nightshift.timer")
	case "windows":
		_ = run(ctx, "schtasks", "/Delete", "/F", "/TN", "nightshift")
	}
	for path := range files {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if runtime.GOOS == "linux" {
		_ = run(ctx, "systemctl", "--user", "daemon-reload")
	}
	return nil
}

// Status reports whether the service is installed and loaded.
func Status(ctx context.Context) (bool, string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err.Error()
	}
	files, err := Render(runtime.GOOS, Spec{}, home)
	if err != nil {
		return false, "no service manager here: run `nightshift run` from cron, or keep `nightshift daemon` running"
	}
	switch runtime.GOOS {
	case "darwin":
		if run(ctx, "launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+Label) == nil {
			return true, "launchd agent " + Label + " runs nightshift every minute"
		}
	case "linux":
		if run(ctx, "systemctl", "--user", "is-active", "--quiet", "nightshift.timer") == nil {
			return true, "systemd user timer nightshift.timer runs nightshift every minute"
		}
	case "windows":
		if run(ctx, "schtasks", "/Query", "/TN", "nightshift") == nil {
			return true, "scheduled task nightshift runs every minute"
		}
		return false, "the background runner is not installed"
	}
	for path := range files {
		if _, err := os.Stat(path); err == nil {
			return false, "installed at " + path + " but not loaded"
		}
	}
	return false, "the background runner is not installed"
}

func run(ctx context.Context, name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %s", name, msg)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func launchdPlist(s Spec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(s.Executable) + `</string>
		<string>run</string>
	</array>
	<key>StartInterval</key>
	<integer>60</integer>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(s.LogFile) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(s.LogFile) + `</string>
`)
	if len(s.Env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, k := range sortedKeys(s.Env) {
			b.WriteString("\t\t<key>" + xmlEscape(k) + "</key>\n\t\t<string>" + xmlEscape(s.Env[k]) + "</string>\n")
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func systemdService(s Spec) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=nightshift: create, push and open what was scheduled\n\n[Service]\nType=oneshot\n")
	b.WriteString("ExecStart=" + systemdQuote(s.Executable) + " run\n")
	for _, k := range sortedKeys(s.Env) {
		b.WriteString("Environment=" + systemdQuote(k+"="+s.Env[k]) + "\n")
	}
	if s.LogFile != "" {
		b.WriteString("StandardOutput=append:" + s.LogFile + "\nStandardError=append:" + s.LogFile + "\n")
	}
	return b.String()
}

// systemdTimer fires every minute and, with Persistent, right after boot or
// resume when a run was missed.
const systemdTimer = `[Unit]
Description=Run nightshift every minute

[Timer]
OnCalendar=minutely
AccuracySec=10s
Persistent=true

[Install]
WantedBy=timers.target
`

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// systemdQuote quotes a word for a unit file: double quotes, backslash
// escapes, and % doubled so it is not read as a specifier.
func systemdQuote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(s)
	return `"` + s + `"`
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
