// Package git runs the git that is installed on the system. nightshift never
// reimplements git: commits, signing, hooks, credentials, SSH keys and
// transports all come from the user's own git and its configuration.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrDetached is returned when HEAD does not point at a branch.
var ErrDetached = errors.New("HEAD is detached; check out a branch first")

// NonInteractive keeps git, its credential helpers and Git Credential Manager
// from waiting for input nobody is there to give.
var NonInteractive = []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"}

// Error is a git invocation that exited unsuccessfully.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", subcommand(e.Args), msg)
}

func (e *Error) Unwrap() error { return e.Err }

func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" || args[i] == "-C" {
			i++
			continue
		}
		if !strings.HasPrefix(args[i], "-") {
			return args[i]
		}
	}
	return strings.Join(args, " ")
}

// Repo is a git working tree.
type Repo struct {
	// Dir is the top-level directory of the working tree.
	Dir string
	// Env is added to the environment of every invocation.
	Env []string
}

// Open finds the working tree that contains dir.
func Open(ctx context.Context, dir string) (*Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	top, err := (&Repo{Dir: abs}).Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git working tree: %w", abs, err)
	}
	return &Repo{Dir: filepath.Clean(top)}, nil
}

// Exec describes one git invocation.
type Exec struct {
	Args  []string
	Env   []string
	Stdin io.Reader
	// Stdout, when set, receives the output instead of it being captured.
	Stdout io.Writer
	// Stderr, when set, receives a copy of the error output as it is written.
	Stderr io.Writer
	// Detach runs git without a controlling terminal, as at night.
	Detach bool
}

// Do runs git and returns its standard output without the trailing newline.
func (r *Repo) Do(ctx context.Context, x Exec) (string, error) {
	cmd := exec.CommandContext(ctx, "git", x.Args...) //nolint:gosec // G204: running git with arguments is this package's job
	cmd.Dir = r.Dir
	cmd.Env = append(append(os.Environ(), r.Env...), x.Env...)
	cmd.Stdin = x.Stdin
	if x.Detach {
		detach(cmd)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	if x.Stdout != nil {
		cmd.Stdout = x.Stdout
	}
	cmd.Stderr = &errb
	if x.Stderr != nil {
		cmd.Stderr = io.MultiWriter(x.Stderr, &errb)
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (%w)", err, ctx.Err())
		}
		return "", &Error{Args: x.Args, Stderr: errb.String(), Err: err}
	}
	return strings.TrimRight(out.String(), "\r\n"), nil
}

// Run runs git with args and returns its trimmed standard output.
func (r *Repo) Run(ctx context.Context, args ...string) (string, error) {
	return r.Do(ctx, Exec{Args: args})
}

// GitDir returns the absolute path of the repository's .git directory.
func (r *Repo) GitDir(ctx context.Context) (string, error) {
	return r.Run(ctx, "rev-parse", "--absolute-git-dir")
}

// Branch returns the branch HEAD points at.
func (r *Repo) Branch(ctx context.Context) (string, error) {
	out, err := r.Run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", ErrDetached
	}
	return out, nil
}

// HeadRef returns the full ref HEAD points at, or "" when it is detached.
func (r *Repo) HeadRef(ctx context.Context) string {
	out, err := r.Run(ctx, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// Resolve returns the commit rev names.
func (r *Repo) Resolve(ctx context.Context, rev string) (string, error) {
	return r.Run(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
}

// TryResolve is Resolve that reports a missing revision as ok == false.
func (r *Repo) TryResolve(ctx context.Context, rev string) (string, bool) {
	sha, err := r.Resolve(ctx, rev)
	return sha, err == nil && sha != ""
}

// Tree returns the tree of a commit.
func (r *Repo) Tree(ctx context.Context, rev string) (string, error) {
	return r.Run(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{tree}")
}

// EmptyTree returns the id of the empty tree in this repository's hash.
func (r *Repo) EmptyTree(ctx context.Context) (string, error) {
	return r.Do(ctx, Exec{Args: []string{"hash-object", "-t", "tree", "--stdin"}, Stdin: strings.NewReader("")})
}

// WriteTree writes the index as a tree and returns its id.
func (r *Repo) WriteTree(ctx context.Context) (string, error) {
	return r.Run(ctx, "write-tree")
}

// ObjectExists reports whether an object is in the object database.
func (r *Repo) ObjectExists(ctx context.Context, oid string) bool {
	_, err := r.Run(ctx, "cat-file", "-e", oid)
	return err == nil
}

// Config returns a configuration value, with ok == false when it is unset.
func (r *Repo) Config(ctx context.Context, key string) (string, bool) {
	out, err := r.Run(ctx, "config", "--get", key)
	return out, err == nil
}

// ConfigBool returns a boolean configuration value; unset is false.
func (r *Repo) ConfigBool(ctx context.Context, key string) bool {
	out, err := r.Run(ctx, "config", "--type=bool", "--get", key)
	return err == nil && out == "true"
}

// SetConfig sets a configuration value in the repository's own config.
func (r *Repo) SetConfig(ctx context.Context, key, value string) error {
	_, err := r.Run(ctx, "config", "--local", key, value)
	return err
}

// Var returns a git logical variable such as GIT_AUTHOR_IDENT or GIT_EDITOR.
func (r *Repo) Var(ctx context.Context, name string) (string, error) {
	return r.Run(ctx, "var", name)
}

// Ident is a name and an email address, as in a commit's author line.
type Ident struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (i Ident) String() string { return fmt.Sprintf("%s <%s>", i.Name, i.Email) }

// ParseIdent parses "Name <email>", optionally followed by a timestamp and
// time zone as git prints them.
func ParseIdent(s string) (Ident, error) {
	lt := strings.IndexByte(s, '<')
	gt := strings.LastIndexByte(s, '>')
	if lt < 0 || gt < lt {
		return Ident{}, fmt.Errorf("identity %q: want Name <email>", s)
	}
	return Ident{Name: strings.TrimSpace(s[:lt]), Email: strings.TrimSpace(s[lt+1 : gt])}, nil
}

// identDate parses the "1700000000 +0100" after an identity.
func identDate(s string) time.Time {
	gt := strings.LastIndexByte(s, '>')
	if gt < 0 {
		return time.Time{}
	}
	f := strings.Fields(s[gt+1:])
	if len(f) != 2 {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return time.Time{}
	}
	t := time.Unix(sec, 0)
	if z, err := time.Parse("-0700", f[1]); err == nil {
		t = t.In(z.Location())
	}
	return t
}

// FormatDate formats t for GIT_AUTHOR_DATE and GIT_COMMITTER_DATE: git's own
// "@<unix seconds> <zone>" form, which no locale can misread.
func FormatDate(t time.Time) string {
	return fmt.Sprintf("@%d %s", t.Unix(), t.Format("-0700"))
}

// CommitTreeOptions describe a commit to create from a tree.
type CommitTreeOptions struct {
	Tree    string
	Parents []string
	Message string
	// Author and Committer default to git's configured identity.
	Author, Committer Ident
	// AuthorDate and CommitterDate default to now.
	AuthorDate, CommitterDate time.Time
	// Sign signs the commit with the configured key (gpg.format decides
	// whether that is OpenPGP, SSH or X.509).
	Sign bool
}

// CommitTree creates a commit object and returns its id. It does not move any
// ref; that is UpdateRef's job.
func (r *Repo) CommitTree(ctx context.Context, o CommitTreeOptions) (string, error) {
	args := []string{"commit-tree"}
	if o.Sign {
		args = append(args, "-S")
	} else {
		args = append(args, "--no-gpg-sign")
	}
	args = append(args, o.Tree)
	for _, p := range o.Parents {
		args = append(args, "-p", p)
	}
	args = append(args, "-F", "-")
	var env []string
	if o.Author.Name != "" || o.Author.Email != "" {
		env = append(env, "GIT_AUTHOR_NAME="+o.Author.Name, "GIT_AUTHOR_EMAIL="+o.Author.Email)
	}
	if o.Committer.Name != "" || o.Committer.Email != "" {
		env = append(env, "GIT_COMMITTER_NAME="+o.Committer.Name, "GIT_COMMITTER_EMAIL="+o.Committer.Email)
	}
	if !o.AuthorDate.IsZero() {
		env = append(env, "GIT_AUTHOR_DATE="+FormatDate(o.AuthorDate))
	}
	if !o.CommitterDate.IsZero() {
		env = append(env, "GIT_COMMITTER_DATE="+FormatDate(o.CommitterDate))
	}
	msg := o.Message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	return r.Do(ctx, Exec{Args: args, Env: env, Stdin: strings.NewReader(msg)})
}

// UpdateRef points ref at newOID. With check, the update only happens while
// ref still points at oldOID; an empty oldOID then means "must not exist".
func (r *Repo) UpdateRef(ctx context.Context, ref, newOID, oldOID string, check bool, reason string) error {
	args := []string{"update-ref"}
	if reason != "" {
		args = append(args, "-m", reason)
	}
	args = append(args, ref, newOID)
	if check {
		args = append(args, oldOID)
	}
	_, err := r.Run(ctx, args...)
	return err
}

// DeleteRef deletes a ref; a ref that is already gone is not an error.
func (r *Repo) DeleteRef(ctx context.Context, ref string) error {
	if !r.RefExists(ctx, ref) {
		return nil
	}
	_, err := r.Run(ctx, "update-ref", "-d", ref)
	return err
}

// RefExists reports whether a ref exists, whatever it points at.
func (r *Repo) RefExists(ctx context.Context, ref string) bool {
	_, err := r.Run(ctx, "show-ref", "--verify", "--quiet", ref)
	return err == nil
}

// Commit is a parsed commit object.
type Commit struct {
	ID         string
	Tree       string
	Parents    []string
	Author     Ident
	AuthorDate time.Time
	Committer  Ident
	Message    string
}

// Subject returns the first line of the message.
func (c *Commit) Subject() string { return Subject(c.Message) }

// Subject returns the first line of a commit message.
func Subject(msg string) string {
	s, _, _ := strings.Cut(strings.TrimLeft(msg, "\n"), "\n")
	return strings.TrimSpace(s)
}

// ReadCommit reads and parses a commit object, message byte for byte.
func (r *Repo) ReadCommit(ctx context.Context, rev string) (*Commit, error) {
	id, err := r.Resolve(ctx, rev)
	if err != nil {
		return nil, fmt.Errorf("%s is not a commit: %w", rev, err)
	}
	raw, err := r.Do(ctx, Exec{Args: []string{"cat-file", "commit", id}})
	if err != nil {
		return nil, err
	}
	// Do trims trailing newlines; messages conventionally end with one.
	head, msg, _ := strings.Cut(raw, "\n\n")
	c := &Commit{ID: id, Message: msg + "\n"}
	for line := range strings.SplitSeq(head, "\n") {
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "tree":
			c.Tree = val
		case "parent":
			c.Parents = append(c.Parents, val)
		case "author":
			c.Author, _ = ParseIdent(val)
			c.AuthorDate = identDate(val)
		case "committer":
			c.Committer, _ = ParseIdent(val)
		}
	}
	return c, nil
}

// RunHook runs a hook the way git commit would, if the repository has one.
// Its output goes to out.
func (r *Repo) RunHook(ctx context.Context, out io.Writer, name string, args ...string) error {
	a := []string{"hook", "run", "--ignore-missing", name}
	if len(args) > 0 {
		a = append(a, "--")
		a = append(a, args...)
	}
	if out == nil {
		out = io.Discard
	}
	_, err := r.Do(ctx, Exec{Args: a, Stdout: out, Stderr: out})
	return err
}

// Stripspace cleans up a commit message as git commit does: trailing
// whitespace and surplus blank lines go, and comment lines too when asked.
func (r *Repo) Stripspace(ctx context.Context, msg string, stripComments bool) (string, error) {
	args := []string{"stripspace"}
	if stripComments {
		args = append(args, "--strip-comments")
	}
	out, err := r.Do(ctx, Exec{Args: args, Stdin: strings.NewReader(msg)})
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	return out + "\n", nil
}

// AddTrailer adds a trailer such as "Signed-off-by: A <a@example.com>"
// unless the message already ends with the same one.
func (r *Repo) AddTrailer(ctx context.Context, msg, trailer string) (string, error) {
	out, err := r.Do(ctx, Exec{
		Args:  []string{"interpret-trailers", "--if-exists", "addIfDifferentNeighbor", "--trailer", trailer},
		Stdin: strings.NewReader(msg),
	})
	if err != nil {
		return "", err
	}
	return out + "\n", nil
}

// RemoteURL returns the fetch URL of a remote, with insteadOf applied.
func (r *Repo) RemoteURL(ctx context.Context, remote string) (string, error) {
	return r.Run(ctx, "remote", "get-url", remote)
}

// PushRemote picks the remote a branch is pushed to: branch.<b>.pushRemote,
// remote.pushDefault, branch.<b>.remote, then fallback.
func (r *Repo) PushRemote(ctx context.Context, branch, fallback string) string {
	for _, key := range []string{"branch." + branch + ".pushRemote", "remote.pushDefault", "branch." + branch + ".remote"} {
		if v, ok := r.Config(ctx, key); ok && v != "" && v != "." {
			return v
		}
	}
	return fallback
}

// RemoteHead returns the default branch a remote advertised at clone or
// `git remote set-head` time, if git knows it.
func (r *Repo) RemoteHead(ctx context.Context, remote string) (string, bool) {
	out, err := r.Run(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD")
	if err != nil {
		return "", false
	}
	return strings.TrimPrefix(out, remote+"/"), true
}

// Push pushes a refspec without prompting for anything.
func (r *Repo) Push(ctx context.Context, remote, refspec string, forceWithLease bool) (string, error) {
	args := []string{"push", "--porcelain"}
	if forceWithLease {
		args = append(args, "--force-with-lease")
	}
	args = append(args, remote, refspec)
	return r.Do(ctx, Exec{Args: args, Env: NonInteractive})
}

// DiffStat summarizes the change between two trees or commits.
func (r *Repo) DiffStat(ctx context.Context, from, to string) (string, error) {
	return r.Run(ctx, "diff", "--stat=72", "--summary", from, to, "--")
}

// ShortStat is the one-line "3 files changed, 10 insertions(+)" summary.
func (r *Repo) ShortStat(ctx context.Context, from, to string) (string, error) {
	out, err := r.Run(ctx, "diff", "--shortstat", from, to, "--")
	return strings.TrimSpace(out), err
}

// StagedAgainst lists, as "M\tpath" lines, how the index differs from rev.
func (r *Repo) StagedAgainst(ctx context.Context, rev string) ([]string, error) {
	out, err := r.Run(ctx, "diff", "--cached", "--name-status", rev, "--")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// Version returns the installed git's version as [major, minor].
func Version(ctx context.Context) ([2]int, string, error) {
	out, err := (&Repo{}).Run(ctx, "version")
	if err != nil {
		return [2]int{}, "", err
	}
	f := strings.Fields(out) // "git version 2.54.0" or "git version 2.39.5 (Apple Git-154)"
	if len(f) < 3 {
		return [2]int{}, out, fmt.Errorf("unexpected git version output %q", out)
	}
	parts := strings.SplitN(f[2], ".", 3)
	var v [2]int
	for i := 0; i < 2 && i < len(parts); i++ {
		v[i], _ = strconv.Atoi(parts[i])
	}
	return v, f[2], nil
}
