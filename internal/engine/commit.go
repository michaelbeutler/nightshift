package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/michaelbeutler/nightshift/internal/git"
	"github.com/michaelbeutler/nightshift/internal/queue"
)

// CommitOptions is `nightshift commit`.
type CommitOptions struct {
	Dir string
	// Message is the commit message; when empty, MessageFunc is asked (in
	// the CLI that opens the editor), and failing that an amended commit
	// keeps its message.
	Message     string
	MessageFunc func(ctx context.Context, req MessageRequest) (string, error)
	// All stages every modified and deleted tracked file first, as git commit -a.
	All bool
	// Paths are staged first (git add), relative to Dir.
	Paths      []string
	Amend      bool
	AllowEmpty bool
	// NoVerify skips the pre-commit and commit-msg hooks.
	NoVerify bool
	Signoff  bool
	// Author overrides the author, as "Name <email>".
	Author string
	// At is when to create the commit; "" picks the next slot.
	At string
	// Sign is "always", "never", or "" to follow the configuration.
	Sign string
}

// MessageRequest is what an editor needs to ask for a commit message.
type MessageRequest struct {
	GitDir  string
	Branch  string
	At      time.Time
	Default string
	Changes []string
}

// CommitResult is a buffered commit.
type CommitResult struct {
	Job     *queue.Job   `json:"job"`
	Amended bool         `json:"amended,omitempty"`
	Bumped  bool         `json:"bumped,omitempty"`
	Stat    string       `json:"stat"`
	Moved   []*queue.Job `json:"moved,omitempty"`
}

// Commit buffers the index as a commit that the runner creates at its
// scheduled time. Hooks run now, while someone is there to see them fail;
// the commit object, its dates and its signature are made at night.
func (e *Engine) Commit(ctx context.Context, o CommitOptions) (*CommitResult, error) {
	repo, err := git.Open(ctx, o.Dir)
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
	if err := p.checkHead(branch); err != nil {
		return nil, err
	}

	prior := p.chain
	var target *queue.Job
	if o.Amend {
		if len(prior) == 0 {
			return nil, ErrNothingToAmend
		}
		target = prior[len(prior)-1]
		if target.Status == queue.StatusRunning {
			return nil, fmt.Errorf("buffered commit %s is being created right now", target.ID)
		}
		prior = prior[:len(prior)-1]
	}
	var parent *queue.Job
	parentRev := p.base
	if len(prior) > 0 {
		parent = prior[len(prior)-1]
		parentRev = parent.Commit.Snapshot
	}
	parentTree, err := treeOf(ctx, repo, parentRev)
	if err != nil {
		return nil, err
	}

	if o.All {
		if _, err := repo.Run(ctx, "add", "--update"); err != nil {
			return nil, err
		}
	}
	if len(o.Paths) > 0 {
		args := []string{"add", "--"}
		for _, path := range o.Paths {
			if !filepath.IsAbs(path) {
				path = filepath.Join(o.Dir, path)
			}
			args = append(args, path)
		}
		if _, err := repo.Run(ctx, args...); err != nil {
			return nil, err
		}
	}
	if !o.NoVerify {
		if err := repo.RunHook(ctx, e.Log, "pre-commit"); err != nil {
			return nil, fmt.Errorf("the pre-commit hook failed (--no-verify skips it): %w", err)
		}
	}
	tree, err := repo.WriteTree(ctx)
	if err != nil {
		return nil, err
	}
	if tree == parentTree && !o.AllowEmpty {
		return nil, ErrNothingToCommit
	}

	var at time.Time
	var bumped bool
	switch {
	case target != nil && o.At == "":
		at = target.At
	case parent != nil:
		at, bumped, err = e.slot(o.At, parent.At)
	default:
		at, bumped, err = e.slot(o.At, time.Time{})
	}
	if err != nil {
		return nil, err
	}

	msg := o.Message
	if msg == "" {
		def := ""
		if target != nil {
			def = target.Commit.Message
		}
		msg = def
		if o.MessageFunc != nil {
			changes, _ := repo.Run(ctx, "diff", "--name-status", parentTree, tree, "--")
			gitDir, _ := repo.GitDir(ctx)
			req := MessageRequest{GitDir: gitDir, Branch: branch, At: at, Default: def}
			if changes != "" {
				req.Changes = strings.Split(changes, "\n")
			}
			if msg, err = o.MessageFunc(ctx, req); err != nil {
				return nil, err
			}
		}
	}
	author, committer, err := idents(ctx, repo, o.Author)
	if err != nil {
		return nil, err
	}
	if o.Signoff {
		if msg, err = repo.AddTrailer(ctx, msg, "Signed-off-by: "+committer.String()); err != nil {
			return nil, err
		}
	}
	if msg, err = e.messageHooks(ctx, repo, msg, o.NoVerify); err != nil {
		return nil, err
	}
	if msg, err = repo.Stripspace(ctx, msg, false); err != nil {
		return nil, err
	}
	if strings.TrimSpace(msg) == "" {
		return nil, ErrEmptyMessage
	}

	var parents []string
	if parentRev != "" {
		parents = []string{parentRev}
	}
	snap, err := repo.CommitTree(ctx, git.CommitTreeOptions{Tree: tree, Parents: parents, Message: msg, Author: author, Committer: committer})
	if err != nil {
		return nil, err
	}

	// Everything slow is done; record the plan under the lock, provided
	// nobody changed the branch's commits meanwhile.
	unlock, err := e.Store.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	fresh, err := e.plan(ctx, repo, branch)
	if err != nil {
		return nil, err
	}
	if !sameIDs(fresh.chain, p.chain) || fresh.head != p.head {
		return nil, errors.New("the branch's buffered commits changed while this one was prepared; run the command again")
	}
	job := target
	if job == nil {
		if job, err = e.newJob(queue.KindCommit, repo, branch); err != nil {
			return nil, err
		}
	}
	job.Status, job.At, job.Attempts, job.Error, job.RetryAt = queue.StatusPending, at, 0, "", time.Time{}
	job.Commit = &queue.CommitSpec{Snapshot: snap, Tree: tree, Message: msg, Author: author, Committer: committer, Sign: o.Sign}
	job.DependsOn = nil
	if parent != nil {
		job.Commit.Parent = parent.ID
		job.DependsOn = []string{parent.ID}
	} else {
		job.Commit.Base = p.base
	}
	if err := repo.UpdateRef(ctx, queue.SnapshotRef(job.ID), snap, "", false, "nightshift: buffer "+git.Subject(msg)); err != nil {
		return nil, err
	}
	if err := e.Store.Save(job); err != nil {
		return nil, err
	}
	jobs := append(without(fresh.jobs, findID(fresh.jobs, job.ID)), job)
	moved := repointPush(jobs, job)
	for _, m := range e.reflow(jobs) {
		if !containsID(moved, m.ID) {
			moved = append(moved, m)
		}
	}
	moved = slicesWithout(moved, job)
	if err := e.saveAll(moved...); err != nil {
		return nil, err
	}
	stat, _ := repo.ShortStat(ctx, parentTree, tree)
	return &CommitResult{Job: job, Amended: target != nil, Bumped: bumped, Stat: stat, Moved: moved}, nil
}

// messageHooks runs prepare-commit-msg and (unless noVerify) commit-msg on
// the message, as git commit does, and returns what they leave.
func (e *Engine) messageHooks(ctx context.Context, repo *git.Repo, msg string, noVerify bool) (string, error) {
	gitDir, err := repo.GitDir(ctx)
	if err != nil {
		return "", err
	}
	path := filepath.Join(gitDir, "NIGHTSHIFT_MSG")
	if err := os.WriteFile(path, []byte(msg), 0o600); err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(path) }()
	if err := repo.RunHook(ctx, e.Log, "prepare-commit-msg", path, "message"); err != nil {
		return "", fmt.Errorf("the prepare-commit-msg hook failed: %w", err)
	}
	if !noVerify {
		if err := repo.RunHook(ctx, e.Log, "commit-msg", path); err != nil {
			return "", fmt.Errorf("the commit-msg hook failed (--no-verify skips it): %w", err)
		}
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

// AdoptOptions is `nightshift adopt`.
type AdoptOptions struct {
	Dir string
	// Since limits adoption to commits not reachable from it (e.g.
	// origin/main); "" adopts every commit no remote has.
	Since string
	At    string
}

// AdoptResult lists the commits turned into buffered commits.
type AdoptResult struct {
	Jobs  []*queue.Job `json:"jobs"`
	Base  string       `json:"base"`
	Moved []*queue.Job `json:"moved,omitempty"`
}

// Adopt turns commits made with plain git commit, not yet pushed anywhere,
// into buffered commits: the branch moves back to where they started (like
// git reset --soft, so the working tree and index keep every change) and
// the runner recreates them at night with night dates and fresh signatures.
func (e *Engine) Adopt(ctx context.Context, o AdoptOptions) (*AdoptResult, error) {
	repo, err := git.Open(ctx, o.Dir)
	if err != nil {
		return nil, err
	}
	branch, err := repo.Branch(ctx)
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
	if len(p.chain) > 0 {
		return nil, fmt.Errorf("branch %s already has buffered commits; adopt works on a branch without them", branch)
	}
	if p.head == "" {
		return nil, fmt.Errorf("branch %s has no commits", branch)
	}

	args := []string{"rev-list", "--reverse", "--topo-order", "--parents", p.head, "--not"}
	if o.Since != "" {
		since, err := repo.Resolve(ctx, o.Since)
		if err != nil {
			return nil, fmt.Errorf("%s is not a commit: %w", o.Since, err)
		}
		args = append(args, since)
	} else {
		args = append(args, "--remotes")
	}
	out, err := repo.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, fmt.Errorf("nothing to adopt: every commit on %s is already on a remote", branch)
	}
	published, err := repo.Run(ctx, "rev-list", p.head, "--not", "--remotes")
	if err != nil {
		return nil, err
	}
	unpushed := map[string]bool{}
	for _, sha := range strings.Fields(published) {
		unpushed[sha] = true
	}
	var shas []string
	base := ""
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 1:
			return nil, fmt.Errorf("cannot adopt %s: it is the branch's first commit", short(f[0]))
		case len(f) > 2:
			return nil, fmt.Errorf("cannot adopt %s: merge commits are not supported", short(f[0]))
		case !unpushed[f[0]]:
			return nil, fmt.Errorf("cannot adopt %s: it is already on a remote, and rewriting it would change published history", short(f[0]))
		case i == 0:
			base = f[1]
		case f[1] != shas[i-1]:
			return nil, fmt.Errorf("cannot adopt %s: the commits do not form a straight line", short(f[0]))
		}
		shas = append(shas, f[0])
	}
	_, committer, err := idents(ctx, repo, "")
	if err != nil {
		return nil, err
	}

	res := &AdoptResult{Base: base}
	var prev *queue.Job
	for i, sha := range shas {
		c, err := repo.ReadCommit(ctx, sha)
		if err != nil {
			return nil, err
		}
		spec, after := o.At, time.Time{}
		if i > 0 {
			spec, after = "", prev.At
		}
		at, _, err := e.slot(spec, after)
		if err != nil {
			return nil, err
		}
		job, err := e.newJob(queue.KindCommit, repo, branch)
		if err != nil {
			return nil, err
		}
		job.At = at
		job.Commit = &queue.CommitSpec{Snapshot: sha, Tree: c.Tree, Message: c.Message, Author: c.Author, Committer: committer}
		if prev == nil {
			job.Commit.Base = base
		} else {
			job.Commit.Parent = prev.ID
			job.DependsOn = []string{prev.ID}
		}
		if err := repo.UpdateRef(ctx, queue.SnapshotRef(job.ID), sha, "", false, "nightshift: adopt "+c.Subject()); err != nil {
			return nil, err
		}
		res.Jobs = append(res.Jobs, job)
		prev = job
	}
	// Move the branch back (git reset --soft): the index and working tree
	// keep the adopted changes, now waiting for the night.
	if err := repo.UpdateRef(ctx, "refs/heads/"+branch, base, p.head, true, fmt.Sprintf("nightshift: adopt %d commits", len(shas))); err != nil {
		return nil, err
	}
	if err := e.saveAll(res.Jobs...); err != nil {
		_ = repo.UpdateRef(ctx, "refs/heads/"+branch, p.head, base, true, "nightshift: undo adopt")
		return nil, err
	}
	jobs := append(slicesClone(p.jobs), res.Jobs...)
	res.Moved = repointPush(jobs, prev)
	for _, m := range e.reflow(jobs) {
		if !containsID(res.Moved, m.ID) && !containsID(res.Jobs, m.ID) {
			res.Moved = append(res.Moved, m)
		}
	}
	return res, e.saveAll(append(res.Moved, res.Jobs...)...)
}

// repointPush makes the branch's pending pushes carry its newest buffered
// commit, and returns them.
func repointPush(jobs []*queue.Job, tail *queue.Job) []*queue.Job {
	var out []*queue.Job
	for _, j := range jobs {
		if j.Kind != queue.KindPush || j.Status != queue.StatusPending || j.Push == nil || j.Push.Head == tail.ID {
			continue
		}
		j.Push.Head, j.Push.Commit = tail.ID, ""
		j.DependsOn = []string{tail.ID}
		out = append(out, j)
	}
	return out
}

func idents(ctx context.Context, repo *git.Repo, author string) (git.Ident, git.Ident, error) {
	cs, err := repo.Var(ctx, "GIT_COMMITTER_IDENT")
	if err != nil {
		return git.Ident{}, git.Ident{}, fmt.Errorf("who are you? set user.name and user.email: %w", err)
	}
	committer, err := git.ParseIdent(cs)
	if err != nil {
		return git.Ident{}, git.Ident{}, err
	}
	if author != "" {
		a, err := git.ParseIdent(author)
		return a, committer, err
	}
	as, err := repo.Var(ctx, "GIT_AUTHOR_IDENT")
	if err != nil {
		return git.Ident{}, git.Ident{}, err
	}
	a, err := git.ParseIdent(as)
	return a, committer, err
}

// treeOf is a revision's tree, or the empty tree for "" (an unborn branch).
func treeOf(ctx context.Context, repo *git.Repo, rev string) (string, error) {
	if rev == "" {
		return repo.EmptyTree(ctx)
	}
	return repo.Tree(ctx, rev)
}

func sameIDs(a, b []*queue.Job) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

func findID(jobs []*queue.Job, id string) *queue.Job {
	for _, j := range jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func containsID(jobs []*queue.Job, id string) bool { return findID(jobs, id) != nil }

func slicesWithout(jobs []*queue.Job, drop *queue.Job) []*queue.Job {
	var out []*queue.Job
	for _, j := range jobs {
		if j.ID != drop.ID {
			out = append(out, j)
		}
	}
	return out
}

func slicesClone(jobs []*queue.Job) []*queue.Job { return append([]*queue.Job(nil), jobs...) }
