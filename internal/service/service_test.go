package service

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderLaunchd(t *testing.T) {
	t.Parallel()
	home := filepath.FromSlash("/Users/ada")
	files, err := Render("darwin", Spec{
		Executable: "/opt/homebrew/bin/nightshift",
		Env:        map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin", "NIGHTSHIFT_HOME": "/tmp/a&b"},
		LogFile:    "/Users/ada/.local/state/nightshift/nightshift.log",
	}, home)
	if err != nil {
		t.Fatal(err)
	}
	plist, ok := files[filepath.Join(home, "Library", "LaunchAgents", Label+".plist")]
	if !ok || len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	for _, want := range []string{
		"<string>" + Label + "</string>",
		"<string>/opt/homebrew/bin/nightshift</string>\n\t\t<string>run</string>",
		"<integer>60</integer>",
		"<key>PATH</key>\n\t\t<string>/opt/homebrew/bin:/usr/bin</string>",
		"<string>/tmp/a&amp;b</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
}

func TestRenderSystemd(t *testing.T) {
	t.Parallel()
	home := filepath.FromSlash("/home/ada")
	files, err := Render("linux", Spec{
		Executable: "/home/ada/go/bin/nightshift",
		Env:        map[string]string{"PATH": "/usr/bin:/bin", "SSH_AUTH_SOCK": "/run/user/1000/ssh 100%.sock"},
		LogFile:    "/home/ada/.local/state/nightshift/nightshift.log",
	}, home)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	unit, timer := files[filepath.Join(dir, "nightshift.service")], files[filepath.Join(dir, "nightshift.timer")]
	if !strings.Contains(unit, `ExecStart="/home/ada/go/bin/nightshift" run`) ||
		!strings.Contains(unit, `Environment="SSH_AUTH_SOCK=/run/user/1000/ssh 100%%.sock"`) ||
		!strings.Contains(unit, "StandardOutput=append:/home/ada/.local/state/nightshift/nightshift.log") {
		t.Errorf("service unit:\n%s", unit)
	}
	if !strings.Contains(timer, "OnCalendar=minutely") || !strings.Contains(timer, "Persistent=true") {
		t.Errorf("timer unit:\n%s", timer)
	}
}

func TestRenderElsewhereSuggestsCron(t *testing.T) {
	t.Parallel()
	_, err := Render("plan9", Spec{Executable: "/bin/nightshift", LogFile: "/tmp/log"}, "/home")
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "* * * * * '/bin/nightshift' run") {
		t.Errorf("Render on an unknown OS: %v", err)
	}
}

func TestCaptureEnv(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"PATH":          "/usr/bin",
		"SSH_AUTH_SOCK": "/private/tmp/com.apple.launchd.abc/Listeners",
		"GNUPGHOME":     "/Users/ada/.gnupg",
		"SECRET_TOKEN":  "nope",
	}
	got := CaptureEnv(func(k string) string { return env[k] })
	if got["PATH"] != "/usr/bin" || got["GNUPGHOME"] != "/Users/ada/.gnupg" || len(got) != 2 {
		t.Errorf("CaptureEnv = %v; the launchd agent socket and unrelated variables must be left out", got)
	}
	env["SSH_AUTH_SOCK"] = "/Users/ada/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"
	if got := CaptureEnv(func(k string) string { return env[k] }); got["SSH_AUTH_SOCK"] == "" {
		t.Error("a stable agent socket (1Password) must be kept")
	}
}
