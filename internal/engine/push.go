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
	"github.com/michaelbeutler/nightshift/internal/schedule"
)

// PushOptions is `nightshift push`.
type PushOptions struct {
	Dir            string
	Remote         string
	Branch         string
	At             string
	ForceWithLease bool
}

// PushResult is a scheduled push.
type PushResult struct {
	Job *queue.Job `json:"job"`
	// Commits is how many buffered commits the push carries.
	Commits int `json:"buffered_commits"`
	// Rescheduled is true when the branch already had a push planned and
	// this call updated it.
	Rescheduled bool         `json:"rescheduled,omitempty"`
	Bumped      bool         `json:"bumped,omitempty"`
	Moved       []*queue.Job `json:"moved,omitempty"`
}

// Push schedules a push of the branch: of its buffered commits, after the
// last of them has been created, or of its existing commits.
func (e *Engine) Push(ctx context.Context, o PushOptions) (*PushResult, error) {
	repo, err := git.Open(ctx, o.Dir)
	if err != nil {
		return nil, err
	}
	branch := o.Branch
	if branch == "" {
		if branch, err = repo.Branch(ctx); err != nil {
			return nil, err
		}
	}
	unlock, err := e.Store.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := e.plan(ctx, repo, branch)
	if err != nil {
		return nil, err
	}
	res, err := e.schedulePush(ctx, repo, branch, p, o)
	if err != nil {
		return nil, err
	}
	return res, e.saveAll(append(res.Moved, res.Job)...)
}

// schedulePush plans a push without saving it, so CreatePR can still place
// it, or drop it when the pull request cannot be scheduled.
func (e *Engine) schedulePush(ctx context.Context, repo *git.Repo, branch string, p *plan, o PushOptions) (*PushResult, error) {
	remote := o.Remote
	if remote == "" {
		remote = repo.PushRemote(ctx, branch, e.Config.Remote)
	}
	if _, err := repo.RemoteURL(ctx, remote); err != nil {
		return nil, fmt.Errorf("remote %q: %w", remote, err)
	}
	if err := p.checkHead(branch); err != nil {
		return nil, err
	}
	var head, commit string
	var after time.Time
	if n := len(p.chain); n > 0 {
		head, after = p.chain[n-1].ID, p.chain[n-1].At
	} else {
		if p.head == "" {
			return nil, fmt.Errorf("branch %s has no commits to push", branch)
		}
		if r, ok := repo.TryResolve(ctx, "refs/remotes/"+remote+"/"+branch); ok && r == p.head {
			return nil, ErrNothingToPush
		}
		commit = p.head
	}

	job := p.find(queue.KindPush, remote)
	res := &PushResult{Commits: len(p.chain), Rescheduled: job != nil}
	var at time.Time
	var err error
	if job != nil && o.At == "" {
		at = job.At
	} else if at, res.Bumped, err = e.slot(o.At, after); err != nil {
		return nil, err
	}
	jobs := p.jobs
	if job == nil {
		if job, err = e.newJob(queue.KindPush, repo, branch); err != nil {
			return nil, err
		}
		jobs = append(slicesClone(jobs), job)
	}
	job.Remote, job.Status, job.At, job.Attempts, job.Error, job.RetryAt = remote, queue.StatusPending, at, 0, "", time.Time{}
	job.Push = &queue.PushSpec{Head: head, Commit: commit, ForceWithLease: o.ForceWithLease}
	job.DependsOn = nil
	if head != "" {
		job.DependsOn = []string{head}
	}
	res.Moved = slicesWithout(e.reflow(jobs), job)
	res.Job = job
	return res, nil
}

// PROptions is `nightshift pr create`.
type PROptions struct {
	Dir    string
	Remote string
	Branch string
	At     string
	Title  string
	Body   string
	// Fill takes the title and body from the commits when they are not given.
	Fill bool
	Base string
	// Draft overrides pr.draft when set.
	Draft     *bool
	Reviewers []string
	Assignees []string
	Labels    []string
	Milestone string
	// Repo is the target repository (owner/name) for a pull request from a fork.
	Repo string
	// Replace replaces a pull request already scheduled for the branch.
	Replace bool
}

// PRResult is a scheduled pull request.
type PRResult struct {
	Job      *queue.Job `json:"job"`
	Push     *queue.Job `json:"push,omitempty"`
	Provider string     `json:"provider"`
	Host     string     `json:"host"`
	Bumped   bool       `json:"bumped,omitempty"`
	// Warning says when nightshift found no way to authenticate yet.
	Warning string `json:"warning,omitempty"`
}

// CreatePR schedules a pull request (a merge request on GitLab), and the
// push it needs if the branch is not on the remote yet.
func (e *Engine) CreatePR(ctx context.Context, o PROptions) (*PRResult, error) {
	repo, err := git.Open(ctx, o.Dir)
	if err != nil {
		return nil, err
	}
	branch := o.Branch
	if branch == "" {
		if branch, err = repo.Branch(ctx); err != nil {
			return nil, err
		}
	}
	remote := o.Remote
	if remote == "" {
		remote = repo.PushRemote(ctx, branch, e.Config.Remote)
	}
	raw, err := repo.RemoteURL(ctx, remote)
	if err != nil {
		return nil, fmt.Errorf("remote %q: %w", remote, err)
	}
	u, err := git.ParseRemoteURL(raw)
	if err != nil {
		return nil, err
	}
	prov, err := provider.For(ctx, u, e.Config.Providers, e.Config.PR.Method, e.Env)
	if err != nil {
		return nil, err
	}

	unlock, err := e.Store.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := e.plan(ctx, repo, branch)
	if err != nil {
		return nil, err
	}
	job := p.find(queue.KindPR, "")
	if job != nil && !o.Replace {
		return nil, fmt.Errorf("%w (%s); pass --replace to replace it", ErrPRScheduled, job.ID)
	}

	res := &PRResult{Provider: prov.Kind(), Host: prov.Host()}
	pushed, err := e.schedulePush(ctx, repo, branch, p, PushOptions{Remote: remote})
	var toSave []*queue.Job
	newPush := false
	switch {
	case errors.Is(err, ErrNothingToPush):
	case err != nil:
		return nil, err
	default:
		res.Push = pushed.Job
		newPush = !pushed.Rescheduled
		toSave = append(slicesClone(pushed.Moved), pushed.Job)
	}

	base := firstNonEmpty(o.Base, e.Config.PR.Base)
	if base == "" {
		base, _ = repo.RemoteHead(ctx, remote)
	}
	if base == branch {
		return nil, fmt.Errorf("the pull request would merge %s into itself; pass --base", branch)
	}
	title, body := o.Title, o.Body
	if o.Fill && (title == "" || body == "") {
		msgs, err := e.messages(ctx, repo, p)
		if err != nil {
			return nil, err
		}
		ft, fb := fill(msgs)
		title, body = firstNonEmpty(title, ft), firstNonEmpty(body, fb)
	}
	if strings.TrimSpace(title) == "" {
		return nil, ErrTitleRequired
	}
	draft := e.Config.PR.Draft
	if o.Draft != nil {
		draft = *o.Draft
	}

	// A pull request waits for its push. When this call plans the push and
	// the pull request has an exact time, the push goes just before it
	// instead, so "open it Monday 07:30" does not push days earlier.
	var at time.Time
	var bumped bool
	if exactAt, ok := e.exactTime(o.At); ok && newPush {
		at, err = e.pushBefore(res.Push, exactAt, p.chain)
	} else {
		var after time.Time
		if res.Push != nil {
			after = res.Push.At
		}
		at, bumped, err = e.slot(o.At, after)
	}
	if err != nil {
		return nil, err
	}
	res.Bumped = bumped
	if job == nil {
		if job, err = e.newJob(queue.KindPR, repo, branch); err != nil {
			return nil, err
		}
	}
	job.Remote, job.Status, job.At, job.Attempts, job.Error, job.RetryAt = remote, queue.StatusPending, at, 0, "", time.Time{}
	job.PR = &queue.PRSpec{
		Title:     strings.TrimSpace(title),
		Body:      body,
		Base:      base,
		Draft:     draft,
		Reviewers: orDefault(o.Reviewers, e.Config.PR.Reviewers),
		Assignees: orDefault(o.Assignees, e.Config.PR.Assignees),
		Labels:    orDefault(o.Labels, e.Config.PR.Labels),
		Milestone: o.Milestone,
		Repo:      o.Repo,
	}
	job.DependsOn = nil
	if res.Push != nil {
		job.DependsOn = []string{res.Push.ID}
	}
	if err := e.saveAll(append(toSave, job)...); err != nil {
		return nil, err
	}
	res.Job = job
	if c := prov.Check(ctx, repo.Dir); !c.OK {
		res.Warning = c.Detail
	}
	return res, nil
}

// messages are the messages of the commits a pull request would carry:
// the buffered ones, or else the commits no remote has yet.
func (e *Engine) messages(ctx context.Context, repo *git.Repo, p *plan) ([]string, error) {
	var msgs []string
	for _, j := range p.chain {
		msgs = append(msgs, j.Commit.Message)
	}
	if len(msgs) > 0 || p.head == "" {
		return msgs, nil
	}
	out, err := repo.Run(ctx, "rev-list", "--reverse", p.head, "--not", "--remotes")
	if err != nil {
		return nil, err
	}
	for _, sha := range strings.Fields(out) {
		c, err := repo.ReadCommit(ctx, sha)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, c.Message)
	}
	return msgs, nil
}

// fill makes a title and body from commit messages, as gh pr create --fill
// does: one commit gives its subject and body; several give the first
// subject and a list of all of them.
func fill(msgs []string) (title, body string) {
	switch len(msgs) {
	case 0:
		return "", ""
	case 1:
		subject, rest, _ := strings.Cut(strings.TrimSpace(msgs[0]), "\n")
		return strings.TrimSpace(subject), strings.TrimSpace(rest)
	}
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString("- " + git.Subject(m) + "\n")
	}
	return git.Subject(msgs[0]), strings.TrimRight(b.String(), "\n")
}

// exactTime resolves spec when it names an exact moment.
func (e *Engine) exactTime(spec string) (time.Time, bool) {
	if spec == "" {
		return time.Time{}, false
	}
	in, err := schedule.Resolve(spec, e.Now(), e.window, e.Rand)
	return in.Time, err == nil && in.Exact
}

// pushBefore moves a push to a spacing before the pull request at prAt, but
// after the last buffered commit it carries, and returns prAt.
func (e *Engine) pushBefore(push *queue.Job, prAt time.Time, chain []*queue.Job) (time.Time, error) {
	push.At = prAt.Add(-e.spacing.Pick(e.Rand))
	if n := len(chain); n > 0 {
		last := chain[n-1].At
		if !prAt.After(last) {
			return time.Time{}, &TooEarlyError{At: prAt, After: last}
		}
		if !push.At.After(last) {
			push.At = last.Add(prAt.Sub(last) / 2) // halfway between, when they are close
		}
	}
	return prAt, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func orDefault(v, def []string) []string {
	if len(v) > 0 {
		return v
	}
	return def
}
