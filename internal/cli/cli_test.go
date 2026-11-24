package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI tests set process environment (NIGHTSHIFT_HOME, git config), so
// they run one after another rather than in parallel.

func sandbox(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitcfg := filepath.Join(base, "gitconfig")
	if err := os.WriteFile(gitcfg, []byte("[init]\n\tdefaultBranch = main\n[user]\n\tname = Ada\n\temail = ada@example.com\n[commit]\n\tgpgSign = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitcfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("NIGHTSHIFT_HOME", filepath.Join(base, "home"))
	t.Setenv("NIGHTSHIFT_NOW", "2026-11-24 17:00")
	t.Setenv("NO_COLOR", "1")
	dir := filepath.Join(base, "work")
	for _, args := range [][]string{
		{"init", "-q", "--bare", filepath.Join(base, "remote.git")},
		{"init", "-q", dir},
		{"-C", dir, "remote", "add", "origin", filepath.Join(base, "remote.git")},
		{"-C", dir, "commit", "-q", "--allow-empty", "-m", "chore: initial"},
		{"-C", dir, "push", "-q", "origin", "main"},
		{"-C", dir, "switch", "-q", "-c", "topic"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func nightshift(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Execute(t.Context(), append([]string{"-C", dir}, args...), strings.NewReader(""), &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestCommitListStatusRun(t *testing.T) {
	dir := sandbox(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := nightshift(t, dir, "--json", "commit", "-m", "feat: a", "a.txt", "--at", "02:15")
	if code != 0 {
		t.Fatalf("commit exited %d: %s %s", code, out, errOut)
	}
	var res struct {
		Job struct {
			ID string `json:"id"`
			At string `json:"at"`
		} `json:"job"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Job.ID == "" || !strings.HasPrefix(res.Job.At, "2026-11-25T02:15:00") {
		t.Fatalf("commit --json = %s (%v)", out, err)
	}

	out, _, code = nightshift(t, dir, "push")
	if code != 0 || !strings.Contains(out, "Scheduled the push of topic to origin") {
		t.Errorf("push: %d %s", code, out)
	}
	out, _, _ = nightshift(t, dir, "list")
	if !strings.Contains(out, res.Job.ID) || !strings.Contains(out, "tonight 02:15") || !strings.Contains(out, "feat: a") {
		t.Errorf("list:\n%s", out)
	}
	out, _, _ = nightshift(t, dir, "status")
	if !strings.Contains(out, "On branch topic") || !strings.Contains(out, "→ origin") {
		t.Errorf("status:\n%s", out)
	}

	t.Setenv("NIGHTSHIFT_NOW", "2026-11-25 06:00")
	out, errOut, code = nightshift(t, dir, "--json", "run")
	var rep struct {
		Ran []struct {
			Error string `json:"error"`
		} `json:"ran"`
	}
	if err := json.Unmarshal([]byte(out), &rep); code != 0 || err != nil || len(rep.Ran) != 2 || rep.Ran[0].Error != "" || rep.Ran[1].Error != "" {
		t.Errorf("run: %d\n%s\n%s", code, out, errOut)
	}
	// Not at a terminal (the service writes to its log file), run prints
	// the timestamped log lines only.
	t.Setenv("NIGHTSHIFT_NOW", "2026-11-25 07:00")
	if out, errOut, _ := nightshift(t, dir, "run"); out != "" || errOut != "" {
		t.Errorf("an idle run under the service printed %q %q", out, errOut)
	}
	if got, _ := exec.Command("git", "-C", dir, "log", "-1", "--format=%ad", "--date=format:%H:%M").Output(); strings.TrimSpace(string(got)) != "02:15" {
		t.Errorf("commit dated %q, want 02:15", got)
	}
}

func TestErrorsAndHints(t *testing.T) {
	dir := sandbox(t)
	_, errOut, code := nightshift(t, dir, "commit", "-m", "feat: nothing")
	if code != 1 || !strings.Contains(errOut, "nothing to commit") {
		t.Errorf("commit with nothing staged: %d %q", code, errOut)
	}
	out, _, code := nightshift(t, dir, "--json", "cancel", "nope")
	if code != 1 || !strings.Contains(out, `"error"`) {
		t.Errorf("--json errors must be JSON on stdout: %d %q", code, out)
	}
	_, errOut, _ = nightshift(t, dir, "show", "zzzz")
	if !strings.Contains(errOut, "hint: `nightshift list --all`") {
		t.Errorf("missing hint: %q", errOut)
	}
}

func TestVersionAndHelpNeedNoConfig(t *testing.T) {
	t.Setenv("NIGHTSHIFT_HOME", t.TempDir())
	t.Setenv("NIGHTSHIFT_SCHEDULE_WINDOW", "broken")
	var out bytes.Buffer
	if code := Execute(t.Context(), []string{"version"}, strings.NewReader(""), &out, &out); code != 0 || !strings.HasPrefix(out.String(), "nightshift ") {
		t.Errorf("version: %d %q", code, out.String())
	}
	out.Reset()
	if code := Execute(t.Context(), []string{"--help"}, strings.NewReader(""), &out, &out); code != 0 || !strings.Contains(out.String(), "Schedule work:") {
		t.Errorf("help: %d %q", code, out.String())
	}
	out.Reset()
	if code := Execute(t.Context(), []string{"list"}, strings.NewReader(""), &out, &out); code != 1 || !strings.Contains(out.String(), "schedule.window") {
		t.Errorf("a broken config must fail with its key: %d %q", code, out.String())
	}
}
