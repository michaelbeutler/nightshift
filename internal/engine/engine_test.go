package engine_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michaelbeutler/nightshift/internal/config"
	"github.com/michaelbeutler/nightshift/internal/engine"
	"github.com/michaelbeutler/nightshift/internal/queue"
)

func TestBufferedCommitsAreCreatedPushedAndDatedAtNight(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.txt", "one\n")
	f.git("add", "a.txt")
	c1 := f.commit(engine.CommitOptions{Message: "feat: one"})
	f.write("a.txt", "two\n")
	c2 := f.commit(engine.CommitOptions{Message: "fix: two\n\nWith a body.", All: true})
	push, err := f.eng.Push(t.Context(), engine.PushOptions{Dir: f.dir})
	if err != nil {
		t.Fatal(err)
	}

	tonight1, tonight4 := time.Date(2026, 11, 25, 1, 0, 0, 0, cet), time.Date(2026, 11, 25, 4, 0, 0, 0, cet)
	if c1.Job.At.Before(tonight1) || !c1.Job.At.Before(tonight4) {
		t.Errorf("first commit at %v, want inside tonight's 01:00-04:00 window", c1.Job.At)
	}
	if gap := c2.Job.At.Sub(c1.Job.At); gap < 3*time.Minute || gap > 15*time.Minute {
		t.Errorf("second commit %v after the first, want the 3m-15m spacing", gap)
	}
	if !push.Job.At.After(c2.Job.At) || push.Commits != 2 {
		t.Errorf("push at %v carrying %d commits, want after %v carrying 2", push.Job.At, push.Commits, c2.Job.At)
	}
	if out := f.git("rev-list", "--count", "main..feature"); out != "0" {
		t.Errorf("buffering moved the branch: %s commits ahead", out)
	}

	if rep := f.runAt(c1.Job.At.Add(-time.Minute)); len(rep.Ran) != 0 {
		t.Fatalf("ran %d jobs before their time", len(rep.Ran))
	}
	if rep := f.runAt(c1.Job.At.Add(time.Second)); len(rep.Ran) != 1 || rep.Ran[0].Error != "" {
		t.Fatalf("at the first slot: %+v", rep.Ran)
	}
	rep := f.runAt(push.Job.At.Add(time.Minute))
	if len(rep.Ran) != 2 {
		t.Fatalf("at the push slot ran %d jobs, want 2: %+v", len(rep.Ran), rep.Ran)
	}
	for _, o := range rep.Ran {
		if o.Error != "" {
			t.Fatalf("%s %s failed: %s", o.Job.Kind, o.Job.ID, o.Error)
		}
	}

	log := strings.Split(f.git("log", "--format=%at %ct %an <%ae>|%s", "main..feature"), "\n")
	want := []struct {
		at      time.Time
		subject string
	}{{c2.Job.At, "fix: two"}, {c1.Job.At, "feat: one"}}
	if len(log) != 2 {
		t.Fatalf("feature has %d new commits, want 2:\n%s", len(log), strings.Join(log, "\n"))
	}
	for i, line := range log {
		head, subject, _ := strings.Cut(line, "|")
		fields := strings.Fields(head)
		ts := strconv.FormatInt(want[i].at.Unix(), 10)
		if fields[0] != ts || fields[1] != ts || subject != want[i].subject || !strings.Contains(head, "Ada Lovelace <ada@example.com>") {
			t.Errorf("commit %d = %q, want author and committer date %s (%v) and subject %q", i, line, ts, want[i].at, want[i].subject)
		}
	}
	if got := f.git("log", "-1", "--format=%B", "feature"); got != "fix: two\n\nWith a body." {
		t.Errorf("message = %q", got)
	}
	if local, remote := f.git("rev-parse", "feature"), f.remoteGit("rev-parse", "feature"); local != remote {
		t.Errorf("remote feature is %s, local %s", remote, local)
	}
	if st := f.git("status", "--porcelain"); st != "" {
		t.Errorf("the morning after, git status is not clean:\n%s", st)
	}
	if refs := f.git("for-each-ref", "refs/nightshift/"); refs != "" {
		t.Errorf("snapshot refs left behind:\n%s", refs)
	}
	if up := f.git("rev-parse", "--abbrev-ref", "feature@{upstream}"); up != "origin/feature" {
		t.Errorf("upstream = %q, want origin/feature", up)
	}
}

func TestCommitNeedsChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.eng.Commit(t.Context(), engine.CommitOptions{Dir: f.dir, Message: "feat: nothing"}); !errors.Is(err, engine.ErrNothingToCommit) {
		t.Errorf("Commit with nothing staged: %v, want ErrNothingToCommit", err)
	}
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	f.commit(engine.CommitOptions{Message: "feat: x"})
	if _, err := f.eng.Commit(t.Context(), engine.CommitOptions{Dir: f.dir, Message: "feat: again"}); !errors.Is(err, engine.ErrNothingToCommit) {
		t.Errorf("Commit with the same index again: %v, want ErrNothingToCommit", err)
	}
	if _, err := f.eng.Commit(t.Context(), engine.CommitOptions{Dir: f.dir, Message: "  \n", AllowEmpty: true}); !errors.Is(err, engine.ErrEmptyMessage) {
		t.Errorf("Commit with a blank message: %v, want ErrEmptyMessage", err)
	}
}

func TestAmendReplacesTheLastBufferedCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	first := f.commit(engine.CommitOptions{Message: "feat: x"})
	f.write("b.txt", "y\n")
	f.git("add", "b.txt")
	amended := f.commit(engine.CommitOptions{Amend: true})
	if amended.Job.ID != first.Job.ID || !amended.Job.At.Equal(first.Job.At) || !amended.Amended {
		t.Errorf("amend made %s at %v, want %s at %v", amended.Job.ID, amended.Job.At, first.Job.ID, first.Job.At)
	}
	if amended.Job.Commit.Message != "feat: x\n" {
		t.Errorf("amend without a message changed it to %q", amended.Job.Commit.Message)
	}
	if amended.Job.Commit.Tree == first.Job.Commit.Tree {
		t.Error("amend kept the old tree")
	}
	jobs, _ := f.eng.Jobs(t.Context(), engine.Filter{})
	if len(jobs) != 1 {
		t.Errorf("amend left %d jobs, want 1", len(jobs))
	}
}

func TestHooksRunWhenBuffering(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks")
	}
	f := newFixture(t)
	f.hook("commit-msg", `printf '\nHooked-By: commit-msg\n' >> "$1"`)
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	res := f.commit(engine.CommitOptions{Message: "feat: x", Signoff: true})
	if !strings.Contains(res.Job.Commit.Message, "Hooked-By: commit-msg") || !strings.Contains(res.Job.Commit.Message, "Signed-off-by: Ada Lovelace <ada@example.com>") {
		t.Errorf("message = %q, want the hook's and the sign-off trailers", res.Job.Commit.Message)
	}

	f.hook("pre-commit", "echo 'lint failed' >&2; exit 1")
	f.write("b.txt", "y\n")
	f.git("add", "b.txt")
	if _, err := f.eng.Commit(t.Context(), engine.CommitOptions{Dir: f.dir, Message: "feat: y"}); err == nil || !strings.Contains(err.Error(), "pre-commit") {
		t.Errorf("Commit with a failing pre-commit hook: %v", err)
	}
	f.commit(engine.CommitOptions{Message: "feat: y", NoVerify: true})
}

func TestBranchMovedUnderBufferedCommits(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	c := f.commit(engine.CommitOptions{Message: "feat: x"})
	f.git("commit", "-q", "-m", "oops: a plain commit")

	f.write("b.txt", "y\n")
	f.git("add", "b.txt")
	var moved *engine.BranchMovedError
	if _, err := f.eng.Commit(t.Context(), engine.CommitOptions{Dir: f.dir, Message: "feat: y"}); !errors.As(err, &moved) {
		t.Fatalf("Commit on a moved branch: %v, want BranchMovedError", err)
	}
	rep := f.runAt(c.Job.At.Add(time.Minute))
	if len(rep.Ran) != 1 || !strings.Contains(rep.Ran[0].Error, "moved") || rep.Ran[0].Job.Status != queue.StatusFailed {
		t.Errorf("the night run on a moved branch: %+v", rep.Ran)
	}
}

func TestAdoptRecreatesPlainCommitsAtNight(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	f.git("commit", "-q", "-m", "feat: made by an agent", "-m", "Body.")
	f.write("b.txt", "y\n")
	f.git("add", "b.txt")
	f.git("commit", "-q", "-m", "fix: also by the agent")
	oldTip := f.git("rev-parse", "HEAD")
	base := f.git("rev-parse", "main")

	res, err := f.eng.Adopt(t.Context(), engine.AdoptOptions{Dir: f.dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Jobs) != 2 || res.Base != base {
		t.Fatalf("adopted %d commits onto %s, want 2 onto %s", len(res.Jobs), res.Base, base)
	}
	if f.git("rev-parse", "HEAD") != base {
		t.Error("adopt did not move the branch back")
	}
	if st := f.git("status", "--porcelain"); st != "A  a.txt\nA  b.txt" {
		t.Errorf("after adopt, status = %q, want both files staged", st)
	}
	f.runAt(res.Jobs[1].At.Add(time.Minute))
	newTip := f.git("rev-parse", "HEAD")
	if newTip == oldTip {
		t.Error("the night run reused the daytime commit instead of recreating it")
	}
	if f.git("rev-parse", "HEAD^{tree}") != f.git("rev-parse", oldTip+"^{tree}") {
		t.Error("the recreated commits do not have the same content")
	}
	if got := f.git("log", "--format=%s|%at", "-2"); got != "fix: also by the agent|"+strconv.FormatInt(res.Jobs[1].At.Unix(), 10)+"\nfeat: made by an agent|"+strconv.FormatInt(res.Jobs[0].At.Unix(), 10) {
		t.Errorf("log = %q", got)
	}
	if st := f.git("status", "--porcelain"); st != "" {
		t.Errorf("after the night run, status = %q", st)
	}
}

func TestAdoptRefusesPublishedCommits(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	f.git("commit", "-q", "-m", "feat: pushed already")
	f.git("push", "-q", "origin", "feature")
	if _, err := f.eng.Adopt(t.Context(), engine.AdoptOptions{Dir: f.dir}); err == nil {
		t.Error("Adopt of pushed commits succeeded")
	}
	if _, err := f.eng.Adopt(t.Context(), engine.AdoptOptions{Dir: f.dir, Since: "main"}); err == nil || !strings.Contains(err.Error(), "already on a remote") {
		t.Errorf("Adopt since main of pushed commits: %v", err)
	}
}

func TestCancelKeepsEarlierCommitsInThePush(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	c1 := f.commit(engine.CommitOptions{Message: "feat: keep"})
	f.write("b.txt", "y\n")
	f.git("add", "b.txt")
	c2 := f.commit(engine.CommitOptions{Message: "feat: drop"})
	f.write("c.txt", "z\n")
	f.git("add", "c.txt")
	c3 := f.commit(engine.CommitOptions{Message: "feat: drop too"})
	push, err := f.eng.Push(t.Context(), engine.PushOptions{Dir: f.dir})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := f.eng.CreatePR(t.Context(), engine.PROptions{Dir: f.dir, Title: "x"})
	if err == nil {
		t.Fatalf("a pull request to a local remote was scheduled: %+v", pr)
	}

	canceled, err := f.eng.Cancel(t.Context(), c2.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(canceled) != 2 || canceled[0].ID != c2.Job.ID || canceled[1].ID != c3.Job.ID {
		t.Errorf("canceled %v, want %s and %s", ids(canceled), c2.Job.ID, c3.Job.ID)
	}
	p := f.job(push.Job.ID)
	if p.Status != queue.StatusPending || p.Push.Head != c1.Job.ID {
		t.Errorf("push is %s carrying %s, want pending carrying %s", p.Status, p.Push.Head, c1.Job.ID)
	}
	if st := f.git("status", "--porcelain"); !strings.Contains(st, "c.txt") {
		t.Error("cancel touched the working tree")
	}
	if refs := f.git("for-each-ref", "--format=%(refname)", "refs/nightshift/"); strings.Contains(refs, c2.Job.ID) {
		t.Error("the canceled snapshot ref is still there")
	}
	if _, err := f.eng.Cancel(t.Context(), c1.Job.ID); err != nil {
		t.Fatal(err)
	}
	if p := f.job(push.Job.ID); p.Status != queue.StatusCanceled {
		t.Errorf("with every buffered commit canceled, the push is %s", p.Status)
	}
}

func TestRescheduleMovesWhatFollows(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	c1 := f.commit(engine.CommitOptions{Message: "feat: x"})
	f.write("b.txt", "y\n")
	f.git("add", "b.txt")
	c2 := f.commit(engine.CommitOptions{Message: "feat: y"})
	push, _ := f.eng.Push(t.Context(), engine.PushOptions{Dir: f.dir})

	moved, err := f.eng.Reschedule(t.Context(), c1.Job.ID[:4], "tomorrow 23:00")
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 3 {
		t.Fatalf("moved %d jobs, want 3", len(moved))
	}
	want := time.Date(2026, 11, 25, 23, 0, 0, 0, cet)
	delta := want.Sub(c1.Job.At)
	if got := f.job(c1.Job.ID).At; !got.Equal(want) {
		t.Errorf("first commit at %v, want %v", got, want)
	}
	if got := f.job(c2.Job.ID).At; !got.Equal(c2.Job.At.Add(delta)) {
		t.Errorf("second commit at %v, want %v", got, c2.Job.At.Add(delta))
	}
	if got := f.job(push.Job.ID).At; !got.Equal(push.Job.At.Add(delta)) {
		t.Errorf("push at %v, want %v", got, push.Job.At.Add(delta))
	}
	if _, err := f.eng.Reschedule(t.Context(), c2.Job.ID, "now"); err != nil {
		t.Fatal(err)
	}
	if got := f.job(c2.Job.ID).At; !got.After(want) {
		t.Errorf("a commit was moved before the one it builds on: %v", got)
	}
}

func TestNewCommitsMoveTheScheduledPush(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	f.commit(engine.CommitOptions{Message: "feat: x"})
	push, _ := f.eng.Push(t.Context(), engine.PushOptions{Dir: f.dir})
	f.write("b.txt", "y\n")
	f.git("add", "b.txt")
	late := f.commit(engine.CommitOptions{Message: "feat: y", At: "tomorrow 05:00"})
	p := f.job(push.Job.ID)
	if p.Push.Head != late.Job.ID || !p.At.After(late.Job.At) {
		t.Errorf("push carries %s at %v, want %s after %v", p.Push.Head, p.At, late.Job.ID, late.Job.At)
	}
	if len(late.Moved) != 1 || late.Moved[0].ID != push.Job.ID {
		t.Errorf("Moved = %v, want the push", ids(late.Moved))
	}
}

func TestSignsAtNightWithSSHKey(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	f := newFixture(t)
	key := filepath.Join(f.base, "signing")
	f.run(f.base, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key)
	allowed := filepath.Join(f.base, "allowed")
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	f.write("../allowed", "ada@example.com "+string(pub))
	f.git("config", "gpg.format", "ssh")
	f.git("config", "user.signingKey", key+".pub")
	f.git("config", "gpg.ssh.allowedSignersFile", allowed)
	f.git("config", "commit.gpgSign", "true")

	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	c := f.commit(engine.CommitOptions{Message: "feat: signed"})
	if out := f.git("cat-file", "commit", c.Job.Commit.Snapshot); strings.Contains(out, "gpgsig") {
		t.Error("the daytime snapshot is signed; signing must wait for the night")
	}
	rep := f.runAt(c.Job.At.Add(time.Minute))
	if len(rep.Ran) != 1 || rep.Ran[0].Error != "" || !strings.Contains(rep.Ran[0].Job.Result.Note, "signed") {
		t.Fatalf("night run: %+v", rep.Ran)
	}
	if got := f.git("log", "-1", "--format=%G?"); got != "G" {
		t.Errorf("signature status %q, want G (good)", got)
	}
	never := f.commitAfter(t, "b.txt", "feat: not signed", "never")
	f.runAt(never.Job.At.Add(time.Minute))
	if got := f.git("log", "-1", "--format=%G?"); got != "N" {
		t.Errorf("with sign=never, signature status %q, want N", got)
	}
}

func (f *fixture) commitAfter(t *testing.T, file, msg, sign string) *engine.CommitResult {
	t.Helper()
	f.write(file, msg+"\n")
	f.git("add", file)
	return f.commit(engine.CommitOptions{Message: msg, Sign: sign})
}

func TestPullRequestThroughTheGitHubAPI(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var calls []string
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /repos/acme/rocket/pulls":
			_ = json.Unmarshal(body, &created)
			_, _ = io.WriteString(w, `{"number":42,"html_url":"https://github.example.com/acme/rocket/pull/42"}`)
		case "POST /repos/acme/rocket/pulls/42/requested_reviewers", "PATCH /repos/acme/rocket/issues/42":
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t)
	cfg := config.Default()
	cfg.Providers = []config.Host{{Host: "github.example.com", Type: "github", API: srv.URL, TokenEnv: "TEST_TOKEN"}}
	f.eng.Config = cfg
	f.eng.Env.Getenv = func(k string) string {
		if k == "TEST_TOKEN" {
			return "tok-123"
		}
		return ""
	}
	f.git("remote", "set-url", "origin", "git@github.example.com:acme/rocket.git")
	f.git("config", "url."+f.remote+".pushInsteadOf", "git@github.example.com:acme/rocket.git")

	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	f.commit(engine.CommitOptions{Message: "feat: rockets\n\nThey fly now."})
	draft := true
	res, err := f.eng.CreatePR(t.Context(), engine.PROptions{
		Dir: f.dir, Fill: true, Base: "main", Draft: &draft,
		Reviewers: []string{"octocat", "acme/core"}, Labels: []string{"enhancement"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Push == nil || res.Provider != "github" || res.Warning != "" || res.Job.PR.Title != "feat: rockets" || res.Job.PR.Body != "They fly now." {
		t.Fatalf("CreatePR = %+v (pr %+v)", res, res.Job.PR)
	}
	if _, err := f.eng.CreatePR(t.Context(), engine.PROptions{Dir: f.dir, Title: "again"}); !errors.Is(err, engine.ErrPRScheduled) {
		t.Errorf("a second pull request: %v, want ErrPRScheduled", err)
	}

	rep := f.runAt(res.Job.At.Add(time.Minute))
	if len(rep.Ran) != 3 {
		t.Fatalf("ran %d jobs, want commit, push and PR: %+v", len(rep.Ran), rep.Ran)
	}
	pr := rep.Ran[2]
	if pr.Error != "" || pr.Job.Result.URL != "https://github.example.com/acme/rocket/pull/42" {
		t.Fatalf("PR job: %+v / %+v", pr, pr.Job.Result)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(calls, ", "); got != "POST /repos/acme/rocket/pulls, POST /repos/acme/rocket/pulls/42/requested_reviewers, PATCH /repos/acme/rocket/issues/42" {
		t.Errorf("API calls: %s", got)
	}
	if created["head"] != "feature" || created["base"] != "main" || created["draft"] != true || created["title"] != "feat: rockets" {
		t.Errorf("created %v", created)
	}
}

func TestFailedPushIsRetriedWithBackoff(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	f.commit(engine.CommitOptions{Message: "feat: x"})
	push, _ := f.eng.Push(t.Context(), engine.PushOptions{Dir: f.dir})
	f.git("remote", "set-url", "origin", filepath.Join(f.base, "gone.git"))

	at := push.Job.At.Add(time.Minute)
	rep := f.runAt(at)
	if len(rep.Ran) != 2 || rep.Ran[1].Error == "" {
		t.Fatalf("first run: %+v", rep.Ran)
	}
	p := f.job(push.Job.ID)
	if p.Status != queue.StatusPending || !p.RetryAt.Equal(at.Add(2*time.Minute)) {
		t.Errorf("after one failure the push is %s, retry at %v", p.Status, p.RetryAt)
	}
	if rep := f.runAt(at.Add(time.Minute)); len(rep.Ran) != 0 {
		t.Error("retried before the backoff ended")
	}
	for i := range 3 {
		f.runAt(at.Add(time.Duration(i+1) * time.Hour))
	}
	if p := f.job(push.Job.ID); p.Status != queue.StatusFailed {
		t.Errorf("after every retry the push is %s, want failed", p.Status)
	}
	f.git("remote", "set-url", "origin", f.remote)
	if _, err := f.eng.Retry(t.Context(), push.Job.ID); err != nil {
		t.Fatal(err)
	}
	if rep := f.runAt(at.Add(5 * time.Hour)); len(rep.Ran) != 1 || rep.Ran[0].Error != "" {
		t.Errorf("after retry: %+v", rep.Ran)
	}
}

func TestMissedJobsMoveToTheNextWindowWhenConfigured(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.eng.Config.Schedule.Missed = "next-window"
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	c := f.commit(engine.CommitOptions{Message: "feat: x"})
	push, _ := f.eng.Push(t.Context(), engine.PushOptions{Dir: f.dir})

	rep := f.runAt(time.Date(2026, 11, 25, 9, 0, 0, 0, cet)) // slept through the night
	if len(rep.Ran) != 0 {
		t.Fatalf("ran %d missed jobs", len(rep.Ran))
	}
	moved := f.job(c.Job.ID).At
	if moved.Day() != 26 || moved.Hour() < 1 || moved.Hour() >= 4 {
		t.Errorf("missed commit moved to %v, want the next night's window", moved)
	}
	if gap := f.job(push.Job.ID).At.Sub(moved); gap != push.Job.At.Sub(c.Job.At) {
		t.Errorf("the push moved by a different amount (gap %v)", gap)
	}
}

func TestCatchUpKeepsNightDates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	c := f.commit(engine.CommitOptions{Message: "feat: x"})
	f.runAt(time.Date(2026, 11, 25, 8, 30, 0, 0, cet)) // the laptop woke up
	if got, want := f.git("log", "-1", "--format=%at"), strconv.FormatInt(c.Job.At.Unix(), 10); got != want {
		t.Errorf("caught-up commit dated %s, want the scheduled %s", got, want)
	}
}

func TestStatusShowsUnbufferedChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	f.commit(engine.CommitOptions{Message: "feat: x"})
	f.write("b.txt", "y\n")
	f.git("add", "b.txt")
	s, err := f.eng.Status(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Branch != "feature" || len(s.Commits) != 1 || len(s.Unbuffered) != 1 || s.Unbuffered[0] != "A\tb.txt" {
		t.Errorf("Status = %+v", s)
	}
}

func TestRunIsExclusive(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	unlock, err := f.eng.Store.TryLockRunner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	rep, err := f.eng.Run(t.Context(), engine.RunOptions{})
	if err != nil || !rep.Busy {
		t.Errorf("Run while another runner works = %+v, %v; want busy", rep, err)
	}
}

func ids(jobs []*queue.Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.ID
	}
	return out
}

func TestExactTimesAreKeptToTheSecond(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	c1 := f.commit(engine.CommitOptions{Message: "feat: x", At: "2026-11-25 02:00:30"})
	want1 := time.Date(2026, 11, 25, 2, 0, 30, 0, cet)
	if !c1.Job.At.Equal(want1) || c1.Bumped {
		t.Fatalf("commit at %v (bumped %v), want exactly %v", c1.Job.At, c1.Bumped, want1)
	}

	f.write("b.txt", "y\n")
	f.git("add", "b.txt")
	var early *engine.TooEarlyError
	if _, err := f.eng.Commit(t.Context(), engine.CommitOptions{Dir: f.dir, Message: "feat: y", At: "2026-11-25 02:00"}); !errors.As(err, &early) {
		t.Fatalf("an exact time before the commit it builds on: %v, want TooEarlyError", err)
	}
	// One second after is fine: an exact time is not stretched to the spacing.
	c2 := f.commit(engine.CommitOptions{Message: "feat: y", At: "25.11.2026 02:00:31"})
	if want := want1.Add(time.Second); !c2.Job.At.Equal(want) {
		t.Errorf("second commit at %v, want exactly %v", c2.Job.At, want)
	}
	if _, err := f.eng.Reschedule(t.Context(), c2.Job.ID, "2026-11-25 01:00"); !errors.As(err, &early) {
		t.Errorf("rescheduling before the commit it builds on: %v, want TooEarlyError", err)
	}

	f.runAt(time.Date(2026, 11, 25, 3, 0, 0, 0, cet))
	if got := f.git("log", "--format=%ad|%cd", "--date=format:%Y-%m-%d %H:%M:%S", "-2"); got != "2026-11-25 02:00:31|2026-11-25 02:00:31\n2026-11-25 02:00:30|2026-11-25 02:00:30" {
		t.Errorf("commit dates:\n%s", got)
	}
}

func TestPullRequestAtAnExactTimePushesJustBefore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.eng.Config.Providers = []config.Host{{Host: "github.example.com", Type: "github"}}
	f.git("remote", "set-url", "origin", "git@github.example.com:acme/rocket.git")
	f.git("config", "url."+f.remote+".pushInsteadOf", "git@github.example.com:acme/rocket.git")
	f.write("a.txt", "x\n")
	f.git("add", "a.txt")
	c := f.commit(engine.CommitOptions{Message: "feat: x"}) // tonight

	var early *engine.TooEarlyError
	if _, err := f.eng.CreatePR(t.Context(), engine.PROptions{Dir: f.dir, Title: "x", At: "2026-11-24 23:00"}); !errors.As(err, &early) {
		t.Fatalf("a pull request before its commit: %v, want TooEarlyError", err)
	}
	if jobs, _ := f.eng.Jobs(t.Context(), engine.Filter{}); len(jobs) != 1 {
		t.Fatalf("a refused pull request left %d jobs behind, want only the commit", len(jobs))
	}

	res, err := f.eng.CreatePR(t.Context(), engine.PROptions{Dir: f.dir, Title: "x", At: "mon 07:30"})
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 30, 7, 30, 0, 0, cet)
	if !res.Job.At.Equal(want) {
		t.Errorf("pull request at %v, want exactly %v", res.Job.At, want)
	}
	if gap := want.Sub(res.Push.At); gap < 3*time.Minute || gap > 15*time.Minute || !res.Push.At.After(c.Job.At) {
		t.Errorf("push at %v, want a few minutes before %v (and after the commit at %v)", res.Push.At, want, c.Job.At)
	}
	if saved := f.job(res.Push.ID); !saved.At.Equal(res.Push.At) {
		t.Errorf("saved push at %v, reported %v", saved.At, res.Push.At)
	}
}
