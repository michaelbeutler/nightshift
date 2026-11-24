// Package engine is nightshift itself: it buffers commits during the day,
// plans each branch's commits, push and pull request into the night window,
// and, when the runner calls, creates, signs, pushes and opens them.
//
// The CLI and the MCP server are thin layers over this package.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/michaelbeutler/nightshift/internal/config"
	"github.com/michaelbeutler/nightshift/internal/git"
	"github.com/michaelbeutler/nightshift/internal/provider"
	"github.com/michaelbeutler/nightshift/internal/queue"
	"github.com/michaelbeutler/nightshift/internal/schedule"
	"github.com/michaelbeutler/nightshift/internal/version"
)

var (
	// ErrNothingToCommit is returned when the index adds nothing to the last
	// buffered commit (or to HEAD).
	ErrNothingToCommit = errors.New("nothing to commit: stage changes with git add, or pass -a")
	// ErrNothingToAmend is returned by --amend on a branch with no buffered commit.
	ErrNothingToAmend = errors.New("no buffered commit on this branch to amend")
	// ErrEmptyMessage is returned for an empty commit message.
	ErrEmptyMessage = errors.New("aborting: the commit message is empty")
	// ErrNothingToPush is returned when the remote already has the branch.
	ErrNothingToPush = errors.New("nothing to push: the remote already has this branch")
	// ErrPRScheduled is returned when the branch already has a pull request planned.
	ErrPRScheduled = errors.New("a pull request is already scheduled for this branch")
	// ErrTitleRequired is returned for a pull request without a title.
	ErrTitleRequired = errors.New("a pull request needs a title: pass --title, or --fill to take it from the commits")
	// ErrNotActive is returned for a job that is done or canceled.
	ErrNotActive = errors.New("the job is no longer pending")
)

// BranchMovedError means the branch is no longer where its buffered commits
// were made: someone committed, pulled or reset in the meantime.
type BranchMovedError struct {
	Branch   string
	Expected string
	Actual   string
}

func (e *BranchMovedError) Error() string {
	return fmt.Sprintf("branch %s moved since its commits were buffered (expected %s, found %s): "+
		"let them run first, or cancel them with `nightshift cancel` and buffer again", e.Branch, short(e.Expected), short(e.Actual))
}

// TooEarlyError means an exact time was asked for a job that has to wait
// for another job scheduled at or after it.
type TooEarlyError struct {
	At    time.Time
	After time.Time
}

func (e *TooEarlyError) Error() string {
	return fmt.Sprintf("%s is not after the step it waits for, at %s: pick a later time, or move that step first (nightshift reschedule)",
		e.At.Format("2006-01-02 15:04:05"), e.After.Format("2006-01-02 15:04:05"))
}

// Engine carries the store, the configuration and everything that touches
// the outside world, so tests can replace any of it.
type Engine struct {
	Store  *queue.Store
	Config *config.Config
	// Now is the clock. Commit dates come from scheduled times, not from it,
	// but it decides what is due.
	Now  func() time.Time
	Rand *rand.Rand
	// Log receives progress lines and hook output. It must not be stdout
	// when serving MCP.
	Log io.Writer
	// Env is what pull request providers use to reach the network and tools.
	Env provider.Env
	// Notify shows a desktop notification; it must not block.
	Notify func(title, body string)

	window  schedule.Window
	spacing schedule.Spacing
}

// New builds an engine with real clock, randomness, network and no logging.
func New(store *queue.Store, cfg *config.Config) (*Engine, error) {
	w, err := schedule.ParseWindow(cfg.Schedule.Window)
	if err != nil {
		return nil, err
	}
	sp, err := schedule.ParseSpacing(cfg.Schedule.Spacing)
	if err != nil {
		return nil, err
	}
	return &Engine{
		Store:   store,
		Config:  cfg,
		Now:     time.Now,
		Rand:    rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())), //nolint:gosec // G404: scheduling jitter, not a secret
		Log:     io.Discard,
		Env:     provider.DefaultEnv("nightshift/" + version.Short()),
		Notify:  func(string, string) {},
		window:  w,
		spacing: sp,
	}, nil
}

// Window is the configured nightly window.
func (e *Engine) Window() schedule.Window { return e.window }

// ParseTime resolves a user-supplied time against the engine's clock and window.
func (e *Engine) ParseTime(spec string) (time.Time, error) {
	return schedule.Parse(spec, e.Now(), e.window, e.Rand)
}

// PinClock fixes the engine's clock at spec, which may be in the past
// (NIGHTSHIFT_NOW, for tests and the demo).
func (e *Engine) PinClock(spec string) error {
	t, err := schedule.ParseInstant(spec, e.Now(), e.window, e.Rand)
	if err != nil {
		return err
	}
	e.Now = func() time.Time { return t }
	return nil
}

// slot decides when a new job runs. spec is what the user asked for (""
// for the default) and after is when the job it waits for runs (zero when
// it waits for nothing).
//
// An exact time (a date and time, a time of day, a delay) is kept to the
// second, and is an error when it is not after the job it waits for. With
// no spec, a job that follows a scheduled one lands a spacing after it, and
// anything else in the next window; a loose time ("now", "tonight", a
// window, a day) that falls before that floor is moved up to it (bumped).
func (e *Engine) slot(spec string, after time.Time) (t time.Time, bumped bool, err error) {
	now := e.Now()
	floor := now
	if !after.IsZero() && after.After(now) {
		floor = after.Add(e.spacing.Pick(e.Rand))
	}
	if spec == "" {
		if !after.IsZero() && after.After(now) {
			return floor, false, nil
		}
		t = e.window.Pick(now, e.Rand)
	} else {
		in, err := schedule.Resolve(spec, now, e.window, e.Rand)
		if err != nil {
			return time.Time{}, false, err
		}
		if in.Exact {
			if !after.IsZero() && !in.Time.After(after) {
				return time.Time{}, false, &TooEarlyError{At: in.Time, After: after}
			}
			return in.Time, false, nil
		}
		t = in.Time
	}
	if t.Before(floor) {
		return floor, spec != "", nil
	}
	return t, false, nil
}

// plan is the state of one branch: its active jobs and where it stands.
type plan struct {
	jobs  []*queue.Job // active jobs on the branch
	chain []*queue.Job // its buffered commits, oldest first
	base  string       // the real commit under the chain; the branch tip when there is none
	head  string       // where the branch points now ("" if it has no commits)
}

func (e *Engine) plan(ctx context.Context, repo *git.Repo, branch string) (*plan, error) {
	all, err := e.Store.All()
	if err != nil {
		return nil, err
	}
	p := &plan{}
	for _, j := range all {
		if j.Repo == repo.Dir && j.Branch == branch && j.Active() {
			p.jobs = append(p.jobs, j)
		}
	}
	p.chain = chain(p.jobs)
	p.head, _ = repo.TryResolve(ctx, "refs/heads/"+branch)
	p.base = p.head
	if len(p.chain) > 0 {
		if p.base, err = e.chainBase(p.chain[0]); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *plan) checkHead(branch string) error {
	if len(p.chain) > 0 && p.head != p.base {
		return &BranchMovedError{Branch: branch, Expected: p.base, Actual: p.head}
	}
	return nil
}

func (p *plan) find(kind queue.Kind, remote string) *queue.Job {
	for _, j := range p.jobs {
		if j.Kind == kind && (remote == "" || j.Remote == remote) && j.Status != queue.StatusRunning {
			return j
		}
	}
	return nil
}

// chain orders a branch's buffered commits by their Parent links. The first
// is the one whose parent is no longer active (or that has none).
func chain(jobs []*queue.Job) []*queue.Job {
	commits := map[string]*queue.Job{}
	for _, j := range jobs {
		if j.Kind == queue.KindCommit && j.Commit != nil {
			commits[j.ID] = j
		}
	}
	child := map[string]*queue.Job{}
	var root *queue.Job
	for _, j := range jobs {
		if commits[j.ID] == nil {
			continue
		}
		if _, ok := commits[j.Commit.Parent]; ok {
			child[j.Commit.Parent] = j
		} else if root == nil {
			root = j
		}
	}
	var out []*queue.Job
	for j := root; j != nil && len(out) <= len(commits); j = child[j.ID] {
		out = append(out, j)
	}
	return out
}

// chainBase is the real commit a chain's first buffered commit goes on.
func (e *Engine) chainBase(root *queue.Job) (string, error) {
	if root.Commit.Parent == "" {
		return root.Commit.Base, nil
	}
	p, err := e.Store.Load(root.Commit.Parent)
	if err != nil {
		return "", err
	}
	if p.Result == nil || p.Result.Commit == "" {
		return "", fmt.Errorf("buffered commit %s waits for %s, which has no commit", root.ID, p.ID)
	}
	return p.Result.Commit, nil
}

// reflow moves any pending job that would run at or before a job it waits
// for to a spacing after it, and returns the jobs it moved.
func (e *Engine) reflow(jobs []*queue.Job) []*queue.Job {
	byID := make(map[string]*queue.Job, len(jobs))
	for _, j := range jobs {
		byID[j.ID] = j
	}
	var moved []*queue.Job
	for range len(jobs) + 1 {
		changed := false
		for _, j := range jobs {
			if j.Status != queue.StatusPending {
				continue
			}
			for _, d := range j.DependsOn {
				dep, ok := byID[d]
				if !ok || !dep.Active() || j.At.After(dep.At) {
					continue
				}
				j.At = dep.At.Add(e.spacing.Pick(e.Rand))
				changed = true
				if !slices.Contains(moved, j) {
					moved = append(moved, j)
				}
			}
		}
		if !changed {
			break
		}
	}
	return moved
}

// dependents returns every active job that waits, directly or not, for id.
func dependents(all []*queue.Job, id string) []*queue.Job {
	var out []*queue.Job
	seen := map[string]bool{id: true}
	for frontier := []string{id}; len(frontier) > 0; frontier = frontier[1:] {
		for _, j := range all {
			if !seen[j.ID] && j.Active() && slices.Contains(j.DependsOn, frontier[0]) {
				seen[j.ID] = true
				out = append(out, j)
				frontier = append(frontier, j.ID)
			}
		}
	}
	return out
}

func (e *Engine) saveAll(jobs ...*queue.Job) error {
	for _, j := range jobs {
		if err := e.Store.Save(j); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) newJob(kind queue.Kind, repo *git.Repo, branch string) (*queue.Job, error) {
	id, err := e.Store.NewID()
	if err != nil {
		return nil, err
	}
	return &queue.Job{ID: id, Kind: kind, Status: queue.StatusPending, Repo: repo.Dir, Branch: branch, Created: e.Now()}, nil
}

func short(sha string) string {
	if sha == "" {
		return "(no commit)"
	}
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}

func without(jobs []*queue.Job, drop *queue.Job) []*queue.Job {
	return slices.DeleteFunc(slices.Clone(jobs), func(j *queue.Job) bool { return j == drop })
}
