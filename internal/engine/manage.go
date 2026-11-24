package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/michaelbeutler/nightshift/internal/git"
	"github.com/michaelbeutler/nightshift/internal/queue"
)

// Filter selects jobs for Jobs.
type Filter struct {
	// Repo limits the list to one working tree.
	Repo string
	// All includes finished and canceled jobs.
	All bool
}

// Jobs lists jobs, soonest first.
func (e *Engine) Jobs(_ context.Context, f Filter) ([]*queue.Job, error) {
	all, err := e.Store.All()
	if err != nil {
		return nil, err
	}
	var out []*queue.Job
	for _, j := range all {
		if (f.Repo == "" || j.Repo == f.Repo) && (f.All || j.Active()) {
			out = append(out, j)
		}
	}
	return out, nil
}

// Job finds a job by its ID or a unique prefix of it.
func (e *Engine) Job(_ context.Context, id string) (*queue.Job, error) {
	return e.Store.Find(id)
}

// Blocked reports whether a pending job waits for a job that failed.
func (e *Engine) Blocked(j *queue.Job) bool {
	for _, d := range j.DependsOn {
		if dep, err := e.Store.Load(d); err == nil && dep.Status == queue.StatusFailed {
			return true
		}
	}
	return false
}

// Cancel cancels a job and what cannot happen without it. Canceling a
// buffered commit also cancels the buffered commits after it (their
// snapshots contain its changes); a push that carried it now carries the
// commit before it, or is canceled with its pull request if none is left.
// Changes always stay in the working tree.
func (e *Engine) Cancel(ctx context.Context, id string) ([]*queue.Job, error) {
	unlock, err := e.Store.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	j, err := e.Store.Find(id)
	if err != nil {
		return nil, err
	}
	if !j.Active() {
		return nil, fmt.Errorf("%s: %w", j.ID, ErrNotActive)
	}
	all, err := e.Store.All()
	if err != nil {
		return nil, err
	}
	j = findID(all, j.ID)

	victims := []*queue.Job{j}
	var repointed []*queue.Job
	if j.Kind == queue.KindCommit {
		for c := j; c != nil; {
			next := (*queue.Job)(nil)
			for _, k := range all {
				if k.Active() && k.Kind == queue.KindCommit && k.Commit.Parent == c.ID {
					next = k
				}
			}
			if next != nil {
				victims = append(victims, next)
			}
			c = next
		}
		// What a push that carried the canceled commits carries instead: the
		// buffered commit before them, or the commit it already became.
		var keepJob, keepCommit string
		if p := findID(all, j.Commit.Parent); p != nil {
			if p.Active() {
				keepJob = p.ID
			} else if p.Status == queue.StatusDone && p.Result != nil {
				keepCommit = p.Result.Commit
			}
		}
		for _, k := range all {
			if k.Kind != queue.KindPush || !k.Active() || !dependsOnAny(k, victims) {
				continue
			}
			switch {
			case keepJob != "":
				k.Push.Head, k.Push.Commit, k.DependsOn = keepJob, "", []string{keepJob}
			case keepCommit != "":
				k.Push.Head, k.Push.Commit, k.DependsOn = "", keepCommit, nil
			default:
				victims = append(victims, k)
				continue
			}
			repointed = append(repointed, k)
		}
	}
	for _, v := range slices.Clone(victims) {
		for _, d := range dependents(all, v.ID) {
			if !containsID(victims, d.ID) && !containsID(repointed, d.ID) {
				victims = append(victims, d)
			}
		}
	}
	for _, v := range victims {
		if v.Status == queue.StatusRunning {
			return nil, fmt.Errorf("%s is running right now; try again in a moment", v.ID)
		}
	}
	for _, v := range victims {
		v.Status = queue.StatusCanceled
		if v.Kind == queue.KindCommit {
			repo := &git.Repo{Dir: v.Repo}
			_ = repo.DeleteRef(ctx, queue.SnapshotRef(v.ID))
		}
	}
	if err := e.saveAll(append(victims, repointed...)...); err != nil {
		return nil, err
	}
	return victims, nil
}

func dependsOnAny(j *queue.Job, on []*queue.Job) bool {
	for _, o := range on {
		if slices.Contains(j.DependsOn, o.ID) {
			return true
		}
	}
	return false
}

// Reschedule moves a pending or failed job to a new time and everything
// that waits for it by the same amount, so a whole branch can move at once
// (reschedule its first commit to "now" to ship it right away).
func (e *Engine) Reschedule(ctx context.Context, id, spec string) ([]*queue.Job, error) {
	if spec == "" {
		return nil, errors.New("say when: tonight, 02:30, tomorrow 1am, +2h, now")
	}
	unlock, err := e.Store.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	j, err := e.Store.Find(id)
	if err != nil {
		return nil, err
	}
	if j.Status != queue.StatusPending && j.Status != queue.StatusFailed {
		return nil, fmt.Errorf("%s is %s: %w", j.ID, j.Status, ErrNotActive)
	}
	all, err := e.Store.All()
	if err != nil {
		return nil, err
	}
	j = findID(all, j.ID)
	var after time.Time
	for _, d := range j.DependsOn {
		if dep := findID(all, d); dep != nil && dep.Active() && dep.At.After(after) {
			after = dep.At
		}
	}
	at, _, err := e.slot(spec, after)
	if err != nil {
		return nil, err
	}
	delta := at.Sub(j.At)
	changed := []*queue.Job{j}
	j.At = at
	for _, d := range dependents(all, j.ID) {
		if d.Status == queue.StatusPending || d.Status == queue.StatusFailed {
			d.At = d.At.Add(delta)
			changed = append(changed, d)
		}
	}
	for _, c := range changed {
		c.Status, c.Error, c.Attempts, c.RetryAt = queue.StatusPending, "", 0, time.Time{}
	}
	for _, m := range e.reflow(all) {
		if !containsID(changed, m.ID) {
			changed = append(changed, m)
		}
	}
	return changed, e.saveAll(changed...)
}

// Retry sends a failed job back to the queue. It keeps its time, so a
// commit keeps its night date and the job runs on the runner's next pass.
func (e *Engine) Retry(ctx context.Context, id string) (*queue.Job, error) {
	unlock, err := e.Store.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	j, err := e.Store.Find(id)
	if err != nil {
		return nil, err
	}
	if j.Status != queue.StatusFailed {
		return nil, fmt.Errorf("%s is %s; only failed jobs can be retried", j.ID, j.Status)
	}
	j.Status, j.Error, j.Attempts, j.RetryAt = queue.StatusPending, "", 0, time.Time{}
	return j, e.Store.Save(j)
}

// Status is what is planned for one branch.
type Status struct {
	Repo    string       `json:"repo"`
	Branch  string       `json:"branch"`
	Head    string       `json:"head,omitempty"`
	Commits []*queue.Job `json:"commits"`
	Push    *queue.Job   `json:"push,omitempty"`
	PR      *queue.Job   `json:"pr,omitempty"`
	// Unbuffered lists staged changes no buffered commit contains yet.
	Unbuffered []string `json:"unbuffered,omitempty"`
	// Moved is set when the branch moved since its commits were buffered.
	Moved string `json:"moved,omitempty"`
	// Elsewhere counts active jobs on the repository's other branches.
	Elsewhere int `json:"elsewhere,omitempty"`
}

// Status describes the current branch of the working tree at dir.
func (e *Engine) Status(ctx context.Context, dir string) (*Status, error) {
	repo, err := git.Open(ctx, dir)
	if err != nil {
		return nil, err
	}
	branch, err := repo.Branch(ctx)
	if err != nil {
		return nil, err
	}
	p, err := e.plan(ctx, repo, branch)
	if err != nil {
		return nil, err
	}
	s := &Status{Repo: repo.Dir, Branch: branch, Head: p.head, Commits: p.chain}
	if s.Commits == nil {
		s.Commits = []*queue.Job{}
	}
	s.Push, s.PR = p.find(queue.KindPush, ""), p.find(queue.KindPR, "")
	if err := p.checkHead(branch); err != nil {
		s.Moved = err.Error()
	}
	rev := p.head
	if n := len(p.chain); n > 0 {
		rev = p.chain[n-1].Commit.Snapshot
	}
	if rev == "" {
		rev, _ = repo.EmptyTree(ctx)
	}
	s.Unbuffered, _ = repo.StagedAgainst(ctx, rev)
	others, err := e.Jobs(ctx, Filter{Repo: repo.Dir})
	if err != nil {
		return nil, err
	}
	for _, j := range others {
		if j.Branch != branch {
			s.Elsewhere++
		}
	}
	return s, nil
}
