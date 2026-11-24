// Package notify shows desktop notifications with whatever the platform
// ships: osascript on macOS, notify-send on Linux. It is best effort: a
// missing tool or a headless session is silently fine.
package notify

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Send shows a notification and returns without waiting long for it.
func Send(title, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		script := "display notification " + appleString(body) + " with title " + appleString(title)
		_ = exec.CommandContext(ctx, "osascript", "-e", script).Run()
	case "linux", "freebsd", "openbsd", "netbsd":
		if _, err := exec.LookPath("notify-send"); err == nil {
			_ = exec.CommandContext(ctx, "notify-send", "--app-name=nightshift", title, body).Run()
		}
	}
}

// appleString quotes s as an AppleScript string literal.
func appleString(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", " ").Replace(s)
	return `"` + s + `"`
}
