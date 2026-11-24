package cli

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/michaelbeutler/nightshift/internal/engine"
	"github.com/michaelbeutler/nightshift/internal/queue"
)

// style colors terminal output with plain ANSI escapes.
type style struct{ on bool }

func (s style) wrap(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s style) bold(t string) string    { return s.wrap("1", t) }
func (s style) dim(t string) string     { return s.wrap("2", t) }
func (s style) red(t string) string     { return s.wrap("31", t) }
func (s style) green(t string) string   { return s.wrap("32", t) }
func (s style) yellow(t string) string  { return s.wrap("33", t) }
func (s style) blue(t string) string    { return s.wrap("34", t) }
func (s style) magenta(t string) string { return s.wrap("35", t) }
func (s style) cyan(t string) string    { return s.wrap("36", t) }

func colorEnabled(w io.Writer, noColor bool) bool {
	switch {
	case noColor, os.Getenv("NO_COLOR") != "", os.Getenv("TERM") == "dumb":
		return false
	case os.Getenv("CLICOLOR_FORCE") == "1", os.Getenv("FORCE_COLOR") != "":
		return true
	}
	return isTerminal(w)
}

func isTerminal(x any) bool {
	f, ok := x.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// when formats a time for people: "tonight 02:07", "tomorrow 09:30",
// "Fri 02:00", or a date when it is further away.
func when(t, now time.Time) string {
	t = t.Local()
	now = now.Local()
	day := dayName(t, now)
	return day + " " + t.Format("15:04")
}

func dayName(t, now time.Time) string {
	y1, m1, d1 := now.Date()
	y2, m2, d2 := t.Date()
	days := int(time.Date(y2, m2, d2, 12, 0, 0, 0, time.UTC).Sub(time.Date(y1, m1, d1, 12, 0, 0, 0, time.UTC)).Hours() / 24)
	switch {
	case days == 0 && t.Hour() >= 18, days == 1 && t.Hour() < 6 && now.Hour() >= 12:
		return "tonight"
	case days == 0:
		return "today"
	case days == 1:
		return "tomorrow"
	case days == -1:
		return "yesterday"
	case days > 1 && days < 7:
		return t.Format("Mon")
	}
	return t.Format("2006-01-02")
}

// relative is "in 8h12m", "now" or "12m ago".
func relative(t, now time.Time) string {
	d := t.Sub(now)
	switch {
	case d > 30*time.Second:
		return "in " + human(d)
	case d < -30*time.Second:
		return human(-d) + " ago"
	}
	return "now"
}

func human(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "<1m"
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h >= 48:
		return fmt.Sprintf("%dd%dh", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func (a *app) when(t time.Time) string {
	now := a.eng.Now()
	return a.st.bold(when(t, now)) + " " + a.st.dim("("+relative(t, now)+")")
}

// state is a job's status for display: pending jobs waiting on a failed
// one are "blocked".
func (a *app) state(j *queue.Job) string {
	switch j.Status {
	case queue.StatusPending:
		if a.eng.Blocked(j) {
			return a.st.yellow("blocked")
		}
		if !j.RetryAt.IsZero() {
			return a.st.yellow("retrying")
		}
		return a.st.dim("pending")
	case queue.StatusRunning:
		return a.st.cyan("running")
	case queue.StatusFailed:
		return a.st.red("failed")
	case queue.StatusDone:
		return a.st.green("done")
	}
	return a.st.dim(string(j.Status))
}

func (a *app) kind(j *queue.Job) string {
	switch j.Kind {
	case queue.KindCommit:
		return a.st.magenta("commit")
	case queue.KindPush:
		return a.st.blue("push")
	case queue.KindPR:
		return a.st.cyan("pr")
	}
	return string(j.Kind)
}

// paddedKind is kind padded to the widest kind, for lines that must align.
func (a *app) paddedKind(j *queue.Job) string {
	return a.kind(j) + strings.Repeat(" ", max(0, len("commit")-len(j.Kind)))
}

// details is the one-line description in listings.
func (a *app) details(j *queue.Job) string {
	switch j.Kind {
	case queue.KindCommit:
		return j.Title()
	case queue.KindPush:
		s := "→ " + j.Remote
		if j.Push != nil && j.Push.ForceWithLease {
			s += " (force-with-lease)"
		}
		return s
	case queue.KindPR:
		if j.PR == nil {
			return ""
		}
		s := fmt.Sprintf("%q", j.PR.Title)
		if j.PR.Base != "" {
			s += " → " + j.PR.Base
		}
		if j.PR.Draft {
			s += " (draft)"
		}
		return s
	}
	return ""
}

// meta is the reviewers/labels line of a pull request.
func meta(pr *queue.PRSpec) string {
	var parts []string
	if len(pr.Reviewers) > 0 {
		parts = append(parts, "reviewers "+strings.Join(pr.Reviewers, ", "))
	}
	if len(pr.Assignees) > 0 {
		parts = append(parts, "assignees "+strings.Join(pr.Assignees, ", "))
	}
	if len(pr.Labels) > 0 {
		parts = append(parts, "labels "+strings.Join(pr.Labels, ", "))
	}
	if pr.Milestone != "" {
		parts = append(parts, "milestone "+pr.Milestone)
	}
	if pr.Draft {
		parts = append(parts, "draft")
	}
	return strings.Join(parts, " · ")
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visibleWidth(s string) int { return utf8.RuneCountInString(ansiRe.ReplaceAllString(s, "")) }

// table prints rows with columns padded to their widest visible cell.
func (a *app) table(header []string, rows [][]string) {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], visibleWidth(c))
		}
	}
	line := func(cells []string, dim bool) {
		var b strings.Builder
		for i, c := range cells {
			if dim {
				c = a.st.dim(c)
			}
			b.WriteString(c)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-visibleWidth(c)+2))
			}
		}
		a.printf("%s\n", strings.TrimRight(b.String(), " "))
	}
	line(header, true)
	for _, r := range rows {
		line(r, false)
	}
}

func (a *app) jobRows(jobs []*queue.Job, showRepo bool) [][]string {
	rows := make([][]string, 0, len(jobs))
	for _, j := range jobs {
		at := when(j.At, a.eng.Now())
		branch := j.Branch
		if showRepo {
			branch = baseName(j.Repo) + ":" + branch
		}
		rows = append(rows, []string{a.st.dim(j.ID), a.kind(j), at, a.state(j), branch, a.details(j)})
	}
	return rows
}

func baseName(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (a *app) printMoved(moved []*queue.Job) {
	for _, m := range moved {
		a.printf("    %s %s %s %s\n", a.st.dim("↳"), a.kind(m), a.st.dim(m.ID), "now at "+a.when(m.At))
	}
}

func checkMark(st style, level string) string {
	switch level {
	case engine.LevelOK:
		return st.green("✓")
	case engine.LevelWarn:
		return st.yellow("!")
	}
	return st.red("✗")
}
