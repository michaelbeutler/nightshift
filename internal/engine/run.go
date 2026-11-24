package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/michaelbeutler/nightshift/internal/git"
	"github.com/michaelbeutler/nightshift/internal/provider"
	"github.com/michaelbeutler/nightshift/internal/queue"
)

// How long one step may take at night before it is given up: a signing
// program waiting for a passphrase nobody types must not hang the runner.
const (
	commitTimeout = 3 * time.Minute
	pushTimeout   = 15 * time.Minute
	prTimeout     = 5 * time.Minute
	// keepFinished is how long done and canceled jobs stay listed.
	keepFinished = 14 * 24 * time.Hour
)

// RunOptions is `nightshift run`.
type RunOptions struct {
	// DryRun lists what is due without running it.
	DryRun bool
}

// Outcome is one job the runner ran.
type Outcome struct {
	Job   *queue.Job `json:"job"`
	Error string     `json:"error,omitempty"`
}

// RunReport is what one pass of the runner did.
type RunReport struct {
	// Busy means another runner was already working; this one did nothing.
	Busy bool         `json:"busy,omitempty"`
	Ran  []Outcome    `json:"ran"`
	Due  []*queue.Job `json:"due,omitempty"`
	Next *queue.Job   `json:"next,omitempty"`
}

// Run runs every job that is due, in order, until none is left: a branch's
// commits, then its push, then its pull request. It is safe to call often
// and from several processes; only one runs at a time.
func (e *Engine) Run(ctx context.Context, o RunOptions) (*RunReport, error) {
	unlock, err := e.Store.TryLockRunner(ctx)
	if errors.Is(err, queue.ErrLocked) {
		return &RunReport{Busy: true}, nil
	}
	if err != nil {
		return nil, err
	}
	defer unlock()

	rep := &RunReport{Ran: []Outcome{}}
	if o.DryRun {
		rep.Due, err = e.due(ctx)
		return rep, err
	}
	if err := e.recover(ctx); err != nil {
		return nil, err
	}
	for ctx.Err() == nil {
		job, err := e.claim(ctx)
		if err != nil {
			return rep, err
		}
		if job == nil {
			break
		}
		result, runErr := e.execute(ctx, job)
		job, err = e.finish(ctx, job.ID, result, runErr)
		if err != nil {
			return rep, err
		}
		out := Outcome{Job: job}
		if runErr != nil {
			out.Error = runErr.Error()
		}
		rep.Ran = append(rep.Ran, out)
	}
	e.prune()
	rep.Next = e.next()
	return rep, ctx.Err()
}

// recover returns jobs a crashed runner left running to the queue. Only
// the holder of the runner lock calls it, so nothing else is running them.
func (e *Engine) recover(ctx context.Context) error {
	unlock, err := e.Store.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	all, err := e.Store.All()
	if err != nil {
		return err
	}
	for _, j := range all {
		if j.Status == queue.StatusRunning {
			j.Status = queue.StatusPending
			e.logf("%s %s: interrupted earlier, back in the queue", j.Kind, j.ID)
			if err := e.Store.Save(j); err != nil {
				return err
			}
		}
	}
	return nil
}

// ready reports whether every job j waits for is done; a canceled one
// means j can never run.
func ready(j *queue.Job, byID map[string]*queue.Job) (ok, canceled bool) {
	for _, d := range j.DependsOn {
		dep := byID[d]
		switch {
		case dep == nil || dep.Status == queue.StatusCanceled:
			return false, true
		case dep.Status != queue.StatusDone:
			return false, false
		}
	}
	return true, false
}

func (e *Engine) due(_ context.Context) ([]*queue.Job, error) {
	all, err := e.Store.All()
	if err != nil {
		return nil, err
	}
	byID := index(all)
	now := e.Now()
	var out []*queue.Job
	for _, j := range all {
		if ok, _ := ready(j, byID); ok && j.Status == queue.StatusPending && !j.At.After(now) && !j.RetryAt.After(now) {
			out = append(out, j)
		}
	}
	return out, nil
}

// claim picks the next due job and marks it running. Jobs the machine
// slept through move to the next window first if schedule.missed says so.
func (e *Engine) claim(ctx context.Context) (*queue.Job, error) {
	unlock, err := e.Store.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	all, err := e.Store.All()
	if err != nil {
		return nil, err
	}
	byID := index(all)
	now := e.Now()
	for _, j := range all {
		if j.Status != queue.StatusPending || j.At.After(now) || j.RetryAt.After(now) {
			continue
		}
		ok, canceled := ready(j, byID)
		if canceled {
			j.Status, j.Error = queue.StatusCanceled, "canceled: a job it waited for was canceled"
			if err := e.Store.Save(j); err != nil {
				return nil, err
			}
			continue
		}
		if !ok {
			continue
		}
		if e.Config.Schedule.Missed == "next-window" && now.Sub(j.At) > e.Config.Schedule.Grace && j.Attempts == 0 {
			at := e.window.Pick(now, e.Rand)
			delta := at.Sub(j.At)
			moved := append([]*queue.Job{j}, dependents(all, j.ID)...)
			for _, m := range moved {
				m.At = m.At.Add(delta)
			}
			e.logf("%s %s: missed its slot, moved to %s", j.Kind, j.ID, at.Format(time.RFC3339))
			if err := e.saveAll(moved...); err != nil {
				return nil, err
			}
			continue
		}
		j.Status = queue.StatusRunning
		j.Attempts++
		if err := e.Store.Save(j); err != nil {
			return nil, err
		}
		return j, nil
	}
	return nil, nil
}

// finish records how a job went. A failed push or pull request goes back
// in the queue with a backoff until runner.retries is used up; a commit
// that fails needs a person (a moved branch, a signing key that needs a
// passphrase) and fails at once.
func (e *Engine) finish(ctx context.Context, id string, result *queue.Result, runErr error) (*queue.Job, error) {
	unlock, err := e.Store.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	j, err := e.Store.Load(id)
	if err != nil {
		return nil, err
	}
	now := e.Now()
	if runErr == nil {
		result.At = now
		j.Status, j.Error, j.Result, j.RetryAt = queue.StatusDone, "", result, time.Time{}
		e.logf("%s %s on %s: %s", j.Kind, j.ID, j.Branch, describe(j))
		if e.Config.Notify == "all" {
			e.Notify("nightshift", describe(j))
		}
		return j, e.Store.Save(j)
	}
	j.Error = runErr.Error()
	if j.Kind != queue.KindCommit && j.Attempts <= e.Config.Runner.Retries {
		j.Status = queue.StatusPending
		j.RetryAt = now.Add(backoff(j.Attempts))
		e.logf("%s %s on %s failed (attempt %d, retrying at %s): %s", j.Kind, j.ID, j.Branch, j.Attempts, j.RetryAt.Format("15:04"), j.Error)
		return j, e.Store.Save(j)
	}
	j.Status = queue.StatusFailed
	e.logf("%s %s on %s failed: %s", j.Kind, j.ID, j.Branch, j.Error)
	if e.Config.Notify != "off" {
		e.Notify("nightshift: "+string(j.Kind)+" failed", j.Branch+": "+firstLine(j.Error))
	}
	return j, e.Store.Save(j)
}

func backoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 2 * time.Minute
	case 2:
		return 10 * time.Minute
	}
	return 30 * time.Minute
}

func (e *Engine) execute(ctx context.Context, j *queue.Job) (*queue.Result, error) {
	repo, err := git.Open(ctx, j.Repo)
	if err != nil {
		return nil, err
	}
	switch j.Kind {
	case queue.KindCommit:
		ctx, cancel := context.WithTimeout(ctx, commitTimeout)
		defer cancel()
		return e.runCommit(ctx, repo, j)
	case queue.KindPush:
		ctx, cancel := context.WithTimeout(ctx, pushTimeout)
		defer cancel()
		return e.runPush(ctx, repo, j)
	case queue.KindPR:
		ctx, cancel := context.WithTimeout(ctx, prTimeout)
		defer cancel()
		return e.runPR(ctx, repo, j)
	}
	return nil, fmt.Errorf("unknown job kind %q", j.Kind)
}

// runCommit creates the real commit: same tree and message as the
// snapshot, dated at the scheduled time, signed now if configured, on top
// of the commit before it, and moves the branch there.
func (e *Engine) runCommit(ctx context.Context, repo *git.Repo, j *queue.Job) (*queue.Result, error) {
	spec := j.Commit
	parent := spec.Base
	if spec.Parent != "" {
		p, err := e.Store.Load(spec.Parent)
		if err != nil {
			return nil, err
		}
		if p.Status != queue.StatusDone || p.Result == nil {
			return nil, fmt.Errorf("the commit before it (%s) has not been created", p.ID)
		}
		parent = p.Result.Commit
	}
	if !repo.ObjectExists(ctx, spec.Tree) {
		return nil, fmt.Errorf("the buffered tree %s is gone from the repository", short(spec.Tree))
	}
	ref := "refs/heads/" + j.Branch
	tip, _ := repo.TryResolve(ctx, ref)
	if tip != parent {
		// An interrupted run may already have made this very commit.
		if c, err := repo.ReadCommit(ctx, ref); err == nil && c.Tree == spec.Tree && c.Message == spec.Message &&
			len(c.Parents) == 1 && c.Parents[0] == parent {
			return &queue.Result{Commit: c.ID}, nil
		}
		return nil, &BranchMovedError{Branch: j.Branch, Expected: parent, Actual: tip}
	}
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	sign := e.shouldSign(ctx, repo, spec.Sign)
	sha, err := repo.CommitTree(ctx, git.CommitTreeOptions{
		Tree: spec.Tree, Parents: parents, Message: spec.Message,
		Author: spec.Author, Committer: spec.Committer,
		AuthorDate: j.At, CommitterDate: j.At,
		Sign: sign,
	})
	if err != nil {
		if sign {
			return nil, fmt.Errorf("signing failed; the key must work without a prompt at night (see nightshift doctor): %w", err)
		}
		return nil, err
	}
	if err := repo.UpdateRef(ctx, ref, sha, parent, true, "nightshift: commit "+git.Subject(spec.Message)); err != nil {
		return nil, fmt.Errorf("move %s: %w", j.Branch, err)
	}
	note := e.syncWorktree(ctx, repo, ref, parent, sha)
	_ = repo.DeleteRef(ctx, queue.SnapshotRef(j.ID))
	if sign {
		note = strings.TrimSpace("signed " + note)
	}
	return &queue.Result{Commit: sha, Note: note}, nil
}

// syncWorktree keeps a checked-out branch's index and files in step when
// they were left at the old commit (someone ran git stash or reset after
// buffering). When the index already holds the buffered changes, the usual
// case, there is nothing to do. It never overwrites local changes.
func (e *Engine) syncWorktree(ctx context.Context, repo *git.Repo, ref, from, to string) string {
	if repo.HeadRef(ctx) != ref || from == "" {
		return ""
	}
	index, err := repo.WriteTree(ctx)
	if err != nil {
		return ""
	}
	fromTree, err1 := repo.Tree(ctx, from)
	toTree, err2 := repo.Tree(ctx, to)
	if err1 != nil || err2 != nil || index != fromTree || fromTree == toTree {
		return ""
	}
	if _, err := repo.Run(ctx, "read-tree", "-m", "-u", from, to); err != nil {
		return "(working tree left as it was: " + firstLine(err.Error()) + ")"
	}
	return "(working tree updated)"
}

func (e *Engine) shouldSign(ctx context.Context, repo *git.Repo, spec string) bool {
	mode := spec
	if mode == "" {
		mode = e.Config.Sign
	}
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	return repo.ConfigBool(ctx, "commit.gpgSign")
}

func (e *Engine) runPush(ctx context.Context, repo *git.Repo, j *queue.Job) (*queue.Result, error) {
	sha := j.Push.Commit
	if j.Push.Head != "" {
		h, err := e.Store.Load(j.Push.Head)
		if err != nil {
			return nil, err
		}
		if h.Status != queue.StatusDone || h.Result == nil {
			return nil, fmt.Errorf("buffered commit %s has not been created", h.ID)
		}
		sha = h.Result.Commit
	}
	if _, err := repo.Push(ctx, j.Remote, sha+":refs/heads/"+j.Branch, j.Push.ForceWithLease); err != nil {
		return nil, err
	}
	// Track the remote branch, as git push -u would, unless already set.
	if _, ok := repo.Config(ctx, "branch."+j.Branch+".remote"); !ok {
		if _, local := repo.TryResolve(ctx, "refs/heads/"+j.Branch); local {
			_ = repo.SetConfig(ctx, "branch."+j.Branch+".remote", j.Remote)
			_ = repo.SetConfig(ctx, "branch."+j.Branch+".merge", "refs/heads/"+j.Branch)
		}
	}
	return &queue.Result{Commit: sha}, nil
}

func (e *Engine) runPR(ctx context.Context, repo *git.Repo, j *queue.Job) (*queue.Result, error) {
	raw, err := repo.RemoteURL(ctx, j.Remote)
	if err != nil {
		return nil, err
	}
	u, err := git.ParseRemoteURL(raw)
	if err != nil {
		return nil, err
	}
	p, err := provider.For(ctx, u, e.Config.Providers, e.Config.PR.Method, e.Env)
	if err != nil {
		return nil, err
	}
	spec := j.PR
	r, err := p.CreatePR(ctx, provider.Request{
		Dir: repo.Dir, Remote: u, Target: spec.Repo,
		Head: j.Branch, Base: spec.Base,
		Title: spec.Title, Body: spec.Body, Draft: spec.Draft,
		Reviewers: spec.Reviewers, Assignees: spec.Assignees, Labels: spec.Labels, Milestone: spec.Milestone,
	})
	if err != nil {
		return nil, err
	}
	res := &queue.Result{URL: r.URL}
	var notes []string
	if r.Existing {
		notes = append(notes, "it already existed")
	}
	notes = append(notes, r.Warnings...)
	res.Note = strings.Join(notes, "; ")
	return res, nil
}

func (e *Engine) prune() {
	all, err := e.Store.All()
	if err != nil {
		return
	}
	for _, j := range all {
		if !j.Active() && time.Since(j.Updated) > keepFinished {
			_ = e.Store.Remove(j.ID)
		}
	}
}

func (e *Engine) next() *queue.Job {
	all, err := e.Store.All()
	if err != nil {
		return nil
	}
	for _, j := range all {
		if j.Status == queue.StatusPending {
			return j
		}
	}
	return nil
}

func (e *Engine) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(e.Log, "%s %s\n", e.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func describe(j *queue.Job) string {
	r := j.Result
	if r == nil {
		return string(j.Status)
	}
	var s string
	switch j.Kind {
	case queue.KindCommit:
		s = "committed " + short(r.Commit) + " " + j.Title()
	case queue.KindPush:
		s = "pushed " + short(r.Commit) + " to " + j.Remote + "/" + j.Branch
	case queue.KindPR:
		s = "opened " + r.URL
	}
	if r.Note != "" {
		s += " (" + r.Note + ")"
	}
	return s
}

func index(all []*queue.Job) map[string]*queue.Job {
	m := make(map[string]*queue.Job, len(all))
	for _, j := range all {
		m[j.ID] = j
	}
	return m
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}
