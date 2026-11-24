package engine_test

import (
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michaelbeutler/nightshift/internal/config"
	"github.com/michaelbeutler/nightshift/internal/engine"
	"github.com/michaelbeutler/nightshift/internal/provider"
	"github.com/michaelbeutler/nightshift/internal/queue"
)

// TestMain isolates git from the developer's own configuration: no global
// signing, hooks, aliases or identity leak into the tests.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nightshift-test-git")
	if err != nil {
		panic(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, []byte("[init]\n\tdefaultBranch = main\n[user]\n\tname = Ada Lovelace\n\temail = ada@example.com\n[commit]\n\tgpgSign = false\n"), 0o600); err != nil {
		panic(err)
	}
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		_ = os.Unsetenv(k)
	}
	_ = os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fixture is a working tree on branch "feature", cloned from a bare remote
// that has "main", with an engine whose clock the test controls.
type fixture struct {
	t      *testing.T
	base   string
	dir    string
	remote string
	eng    *engine.Engine

	mu  sync.Mutex
	now time.Time
}

var cet = time.FixedZone("CET", 3600)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t: t, base: base,
		dir:    filepath.Join(base, "work"),
		remote: filepath.Join(base, "remote.git"),
		now:    time.Date(2026, 11, 24, 17, 0, 0, 0, cet),
	}
	f.run(base, "git", "init", "-q", "--bare", f.remote)
	f.run(base, "git", "init", "-q", f.dir)
	f.git("remote", "add", "origin", f.remote)
	f.write("README.md", "hello\n")
	f.git("add", "README.md")
	f.git("commit", "-q", "-m", "chore: initial")
	f.git("push", "-q", "origin", "main")
	f.git("switch", "-q", "-c", "feature")

	store, err := queue.Open(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(store, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	eng.Now = f.clock
	eng.Rand = rand.New(rand.NewPCG(7, 11))
	eng.Env = provider.Env{
		HTTP:      http.DefaultClient,
		LookPath:  func(string) (string, error) { return "", exec.ErrNotFound },
		Run:       noCommands,
		Getenv:    func(string) string { return "" },
		UserAgent: "nightshift-test",
	}
	f.eng = eng
	return f
}

func noCommands(context.Context, string, []string, io.Reader, string, ...string) (string, string, error) {
	return "", "", exec.ErrNotFound
}

func (f *fixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fixture) setNow(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}

func (f *fixture) run(dir, name string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) git(args ...string) string {
	f.t.Helper()
	return f.run(f.dir, "git", args...)
}

func (f *fixture) remoteGit(args ...string) string {
	f.t.Helper()
	return f.run(f.base, "git", append([]string{"--git-dir", f.remote}, args...)...)
}

func (f *fixture) write(name, content string) {
	f.t.Helper()
	path := filepath.Join(f.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) commit(o engine.CommitOptions) *engine.CommitResult {
	f.t.Helper()
	o.Dir = f.dir
	res, err := f.eng.Commit(f.t.Context(), o)
	if err != nil {
		f.t.Fatalf("Commit: %v", err)
	}
	return res
}

func (f *fixture) runAt(t time.Time) *engine.RunReport {
	f.t.Helper()
	f.setNow(t)
	rep, err := f.eng.Run(f.t.Context(), engine.RunOptions{})
	if err != nil {
		f.t.Fatalf("Run: %v", err)
	}
	return rep
}

func (f *fixture) job(id string) *queue.Job {
	f.t.Helper()
	j, err := f.eng.Store.Load(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *fixture) hook(name, script string) {
	f.t.Helper()
	path := filepath.Join(f.dir, ".git", "hooks", name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
}
