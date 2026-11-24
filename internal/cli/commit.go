package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/michaelbeutler/nightshift/internal/engine"
	"github.com/michaelbeutler/nightshift/internal/git"
)

const atHelp = `when. Exact, kept to the second: 2026-12-01 02:00[:30], 01.12.2026 02:00,
dec 1 2am, tomorrow 01:30, fri 02:00, 02:30, +2h, @<unix seconds>, optionally
followed by a zone (UTC, +01:00, Europe/Zurich). Loose, nightshift picks the
moment: tonight (the default: schedule.window), now, 01:00-03:00, 2026-12-01, fri`

func (a *app) commitCmd() *cobra.Command {
	var (
		o            engine.CommitOptions
		messages     []string
		file         string
		sign, noSign bool
	)
	cmd := &cobra.Command{
		Use:   "commit [flags] [--] [<pathspec>...]",
		Short: "Buffer a commit that is created tonight",
		Long: `Buffer the staged changes as a commit that nightshift creates at its scheduled
time, dated then and signed then (if you sign commits).

Hooks run now: pre-commit, prepare-commit-msg and commit-msg see your changes
while you are there to fix them. Your working tree and index are left as they
are, so you keep working; once the night run has made the commit, the branch
moves to it and git status is clean again.

Paths given as arguments are staged first (git add), then the whole index is
buffered. Each further commit on the branch goes a few minutes after the
previous one; a push already scheduled for the branch moves after it.`,
		Example: `  nightshift commit -m "fix: handle empty input"
  nightshift commit -am "feat: parse nested blocks" --at 02:30
  nightshift commit src/parser.go -m "refactor: split the parser"
  nightshift commit --amend -m "feat: parse nested blocks and lists"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			o.Dir, o.Paths = a.workdir(), args
			o.Message = strings.Join(messages, "\n\n")
			if file != "" {
				data, err := readInput(file, a.in)
				if err != nil {
					return err
				}
				o.Message = string(data)
			}
			switch {
			case sign:
				o.Sign = "always"
			case noSign:
				o.Sign = "never"
			}
			if o.Message == "" && !a.jsonOut && isTerminal(a.in) && isTerminal(a.errOut) {
				o.MessageFunc = a.editMessage
			}
			res, err := eng.Commit(cmd.Context(), o)
			if err != nil {
				return err
			}
			return a.emit(res, func() {
				verb := "Buffered"
				if res.Amended {
					verb = "Amended"
				}
				a.printf("%s %s %s on %s for %s\n", a.st.magenta("☾"), verb, a.st.bold(res.Job.ID), a.st.cyan(res.Job.Branch), a.when(res.Job.At))
				a.printf("    %s\n", res.Job.Title())
				if res.Stat != "" {
					a.printf("    %s\n", a.st.dim(res.Stat))
				}
				if res.Bumped {
					a.printf("    %s\n", a.st.yellow("moved later, after the commit before it"))
				}
				a.printMoved(res.Moved)
			})
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&messages, "message", "m", nil, "the commit message (repeat for more paragraphs)")
	f.StringVarP(&file, "file", "F", "", "read the commit message from a file (- for stdin)")
	f.BoolVarP(&o.All, "all", "a", false, "stage modified and deleted tracked files first")
	f.BoolVar(&o.Amend, "amend", false, "replace the branch's last buffered commit")
	f.BoolVar(&o.AllowEmpty, "allow-empty", false, "buffer a commit that changes nothing")
	f.BoolVarP(&o.NoVerify, "no-verify", "n", false, "skip the pre-commit and commit-msg hooks")
	f.BoolVarP(&o.Signoff, "signoff", "s", false, "add a Signed-off-by trailer")
	f.StringVar(&o.Author, "author", "", `override the author, "Name <email>"`)
	f.StringVar(&o.At, "at", "", atHelp)
	f.BoolVarP(&sign, "gpg-sign", "S", false, "sign it, whatever commit.gpgSign says")
	f.BoolVar(&noSign, "no-gpg-sign", false, "do not sign it, whatever commit.gpgSign says")
	cmd.MarkFlagsMutuallyExclusive("message", "file")
	cmd.MarkFlagsMutuallyExclusive("gpg-sign", "no-gpg-sign")
	return cmd
}

func (a *app) adoptCmd() *cobra.Command {
	var o engine.AdoptOptions
	cmd := &cobra.Command{
		Use:   "adopt [<since>]",
		Short: "Turn commits you already made into buffered ones",
		Long: `Adopt commits made with plain git commit that no remote has yet: the branch
moves back to where they started (like git reset --soft, so every change stays
in your index and working tree), and nightshift recreates each one tonight with
the same message, author and content, a night date and a fresh signature.

Without <since>, every commit on the branch that is on no remote is adopted.
Merge commits are not.`,
		Example: `  nightshift adopt
  nightshift adopt origin/main --at 01:30`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := a.engine()
			if err != nil {
				return err
			}
			o.Dir = a.workdir()
			if len(args) == 1 {
				o.Since = args[0]
			}
			res, err := eng.Adopt(cmd.Context(), o)
			if err != nil {
				return err
			}
			return a.emit(res, func() {
				a.printf("%s Adopted %d commit(s); the branch is back at %s\n", a.st.magenta("☾"), len(res.Jobs), short(res.Base))
				for _, j := range res.Jobs {
					a.printf("    %s %s  %s\n", a.st.dim(j.ID), when(j.At, eng.Now()), j.Title())
				}
				a.printMoved(res.Moved)
			})
		},
	}
	cmd.Flags().StringVar(&o.At, "at", "", "when the first of them is created; the rest follow it ("+atHelp+")")
	return cmd
}

// editMessage opens the user's git editor on a message template.
func (a *app) editMessage(ctx context.Context, r engine.MessageRequest) (string, error) {
	repo, err := git.Open(ctx, a.workdir())
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(r.Default)
	if r.Default == "" {
		b.WriteString("\n")
	}
	b.WriteString("\n# Please enter the commit message for your changes. Lines starting\n" +
		"# with '#' will be ignored, and an empty message aborts the commit.\n#\n")
	_, _ = fmt.Fprintf(&b, "# nightshift creates this commit on %s at %s.\n", r.Branch, r.At.Local().Format("Mon Jan 2 15:04 MST"))
	if len(r.Changes) > 0 {
		b.WriteString("#\n# Changes to be committed:\n")
		for _, c := range r.Changes {
			b.WriteString("#\t" + nameStatus(c) + "\n")
		}
	}
	text, err := a.edit(ctx, repo, filepath.Join(r.GitDir, "NIGHTSHIFT_EDITMSG"), b.String())
	if err != nil {
		return "", err
	}
	return repo.Stripspace(ctx, text, true)
}

// edit writes content to path, opens $GIT_EDITOR on it and returns the result.
func (a *app) edit(ctx context.Context, repo *git.Repo, path, content string) (string, error) {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { //nolint:gosec // G703: path is in the repository's own .git directory
		return "", err
	}
	defer func() { _ = os.Remove(path) }()
	editor, err := repo.Var(ctx, "GIT_EDITOR")
	if err != nil || editor == "" {
		return "", errors.New("no editor configured: pass -m, or set core.editor")
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", editor+` "`+path+`"`) //nolint:gosec // G204: the user's own configured editor, as git runs it
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", editor+` "$@"`, editor, path) //nolint:gosec // G204: the user's own configured editor, as git runs it
	}
	cmd.Dir = repo.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("the editor failed: %w", err)
	}
	data, err := os.ReadFile(path)
	return string(data), err
}

func nameStatus(line string) string {
	status, rest, _ := strings.Cut(line, "\t")
	label := map[byte]string{'A': "new file", 'M': "modified", 'D': "deleted", 'R': "renamed", 'C': "copied", 'T': "typechange"}[status[0]]
	if label == "" {
		label = status
	}
	return fmt.Sprintf("%-12s%s", label+":", strings.ReplaceAll(rest, "\t", " -> "))
}

func readInput(path string, stdin io.Reader) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(path)
}

func short(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}
