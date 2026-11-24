// Package queue stores nightshift's jobs: one JSON file per job in the state
// directory, each written atomically, with a lock that serializes changes
// and a second one that lets only one runner work at a time.
package queue

import (
	"time"

	"github.com/michaelbeutler/nightshift/internal/git"
)

// Kind is what a job does when it runs.
type Kind string

// The three kinds of job, in the order a branch's jobs usually run.
const (
	// KindCommit turns a buffered snapshot into a real, dated (and, when
	// configured, signed) commit on its branch.
	KindCommit Kind = "commit"
	// KindPush pushes a branch to a remote.
	KindPush Kind = "push"
	// KindPR opens a pull request (GitHub) or merge request (GitLab).
	KindPR Kind = "pr"
)

// Status is where a job is in its life.
type Status string

// Job statuses. Pending, running and failed jobs are active: they still
// belong to their branch's plan. Done and canceled jobs are history.
const (
	StatusPending  Status = "pending"
	StatusRunning  Status = "running"
	StatusDone     Status = "done"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
)

// Job is one scheduled step.
type Job struct {
	ID     string `json:"id"`
	Kind   Kind   `json:"kind"`
	Status Status `json:"status"`
	// Repo is the top-level directory of the working tree the job belongs to.
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Remote string `json:"remote,omitempty"`
	// At is when the job runs. For a commit it is also the commit's author
	// and committer date, even when the job runs late (the machine was asleep).
	At time.Time `json:"at"`
	// RetryAt holds a failed push or pull request back until a backoff ends.
	RetryAt   time.Time `json:"retry_at,omitzero"`
	DependsOn []string  `json:"depends_on,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	Attempts  int       `json:"attempts,omitempty"`
	Error     string    `json:"error,omitempty"`

	Commit *CommitSpec `json:"commit,omitempty"`
	Push   *PushSpec   `json:"push,omitempty"`
	PR     *PRSpec     `json:"pr,omitempty"`
	Result *Result     `json:"result,omitempty"`
}

// CommitSpec is a buffered commit: everything needed to create it later.
type CommitSpec struct {
	// Snapshot is a provisional, unsigned commit with the same tree and
	// message, kept at SnapshotRef(ID) so that git gc cannot collect the
	// tree and `git show` can display it. It is never pushed.
	Snapshot string `json:"snapshot"`
	Tree     string `json:"tree"`
	// Base is the real commit the first buffered commit of a branch goes on
	// top of ("" on a branch with no commits yet). Later ones name Parent.
	Base string `json:"base,omitempty"`
	// Parent is the ID of the previous buffered commit on the branch.
	Parent    string    `json:"parent,omitempty"`
	Message   string    `json:"message"`
	Author    git.Ident `json:"author"`
	Committer git.Ident `json:"committer"`
	// Sign is "always", "never", or "" to follow the configuration.
	Sign string `json:"sign,omitempty"`
}

// PushSpec is a scheduled push of a branch.
type PushSpec struct {
	// Head is the ID of the last buffered commit to push. When the branch
	// had no buffered commits, Commit names the existing commit instead.
	Head           string `json:"head,omitempty"`
	Commit         string `json:"commit,omitempty"`
	ForceWithLease bool   `json:"force_with_lease,omitempty"`
}

// PRSpec is a scheduled pull or merge request.
type PRSpec struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	// Base is the target branch; "" means the repository's default branch.
	Base      string   `json:"base,omitempty"`
	Draft     bool     `json:"draft,omitempty"`
	Reviewers []string `json:"reviewers,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Milestone string   `json:"milestone,omitempty"`
	// Repo is the target repository path (owner/name) when it is not the
	// one the branch is pushed to, as with a pull request from a fork.
	Repo string `json:"repo,omitempty"`
}

// Result is what a finished job produced.
type Result struct {
	Commit string    `json:"commit,omitempty"`
	URL    string    `json:"url,omitempty"`
	Note   string    `json:"note,omitempty"`
	At     time.Time `json:"at"`
}

// Active reports whether the job is still part of its branch's plan.
func (j *Job) Active() bool {
	return j.Status == StatusPending || j.Status == StatusRunning || j.Status == StatusFailed
}

// Title is a one-line description of what the job does.
func (j *Job) Title() string {
	switch j.Kind {
	case KindCommit:
		if j.Commit != nil {
			return git.Subject(j.Commit.Message)
		}
	case KindPush:
		return "push " + j.Branch + " to " + j.Remote
	case KindPR:
		if j.PR != nil {
			return j.PR.Title
		}
	}
	return string(j.Kind)
}

// SnapshotRef is where a buffered commit's snapshot is kept until it runs.
func SnapshotRef(id string) string { return "refs/nightshift/commits/" + id }
