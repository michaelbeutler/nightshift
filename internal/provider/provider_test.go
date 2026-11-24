package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/michaelbeutler/nightshift/internal/config"
	"github.com/michaelbeutler/nightshift/internal/git"
)

// fakeTools records commands and answers them from a table keyed by the
// command line's start ("gh auth status", "glab mr create", ...).
type fakeTools struct {
	mu      sync.Mutex
	calls   []string
	stdin   []string
	answers map[string]struct{ out, errOut string }
	failing map[string]bool
}

func (f *fakeTools) env() Env {
	return Env{
		HTTP: http.DefaultClient,
		LookPath: func(name string) (string, error) {
			for k := range f.answers {
				if strings.HasPrefix(k, name+" ") {
					return "/usr/bin/" + name, nil
				}
			}
			return "", exec.ErrNotFound
		},
		Run: func(_ context.Context, _ string, _ []string, stdin io.Reader, name string, args ...string) (string, string, error) {
			line := name + " " + strings.Join(args, " ")
			in := ""
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				in = string(b)
			}
			f.mu.Lock()
			f.calls = append(f.calls, line)
			f.stdin = append(f.stdin, in)
			f.mu.Unlock()
			for prefix, a := range f.answers {
				if strings.HasPrefix(line, prefix) {
					if f.failing[prefix] {
						return a.out, a.errOut, errors.New("exit status 1")
					}
					return a.out, a.errOut, nil
				}
			}
			return "", "", exec.ErrNotFound
		},
		Getenv:    func(string) string { return "" },
		UserAgent: "test",
	}
}

func TestDetect(t *testing.T) {
	t.Parallel()
	hosts := []config.Host{{Host: "code.example.com", Type: "gitlab"}}
	tools := &fakeTools{answers: map[string]struct{ out, errOut string }{
		"ssh -G work": {out: "user git\nhostname github.com\nport 22\n"},
	}}
	cases := map[string]string{
		"git@github.com:a/b.git":                  "github",
		"https://github.com/a/b":                  "github",
		"git@ssh.github.com:a/b.git":              "github",
		"https://acme.ghe.com/a/b":                "github",
		"https://github.acme.internal/a/b":        "github",
		"git@gitlab.com:group/sub/p.git":          "gitlab",
		"https://gitlab.acme.io/group/p":          "gitlab",
		"ssh://git@code.example.com:2222/g/p.git": "gitlab",
		"work:a/b.git":                            "github",
	}
	for raw, want := range cases {
		u, _ := git.ParseRemoteURL(raw)
		kind, h, err := Detect(t.Context(), u, hosts, tools.env())
		if err != nil || kind != want {
			t.Errorf("Detect(%s) = %q, %v; want %q", raw, kind, err, want)
		}
		if raw == "work:a/b.git" && h.Host != "github.com" {
			t.Errorf("an SSH alias resolved to host %q, want github.com", h.Host)
		}
	}
	u, _ := git.ParseRemoteURL("git@code.unknown.org:a/b.git")
	if _, _, err := Detect(t.Context(), u, hosts, tools.env()); !errors.Is(err, ErrUnknownHost) {
		t.Errorf("unknown host: %v", err)
	}
	u, _ = git.ParseRemoteURL("/srv/repo.git")
	if _, _, err := Detect(t.Context(), u, hosts, tools.env()); !errors.Is(err, ErrLocalRemote) {
		t.Errorf("local remote: %v", err)
	}
}

func TestGitHubCLI(t *testing.T) {
	t.Parallel()
	tools := &fakeTools{answers: map[string]struct{ out, errOut string }{
		"gh auth status": {},
		"gh pr create":   {out: "Creating pull request...\nhttps://github.com/acme/rocket/pull/7\n"},
	}}
	u, _ := git.ParseRemoteURL("git@github.com:me/rocket.git")
	p, err := For(t.Context(), u, nil, "auto", tools.env())
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.CreatePR(t.Context(), Request{
		Remote: u, Target: "acme/rocket", Head: "feat", Base: "main", Title: "T", Body: "B\nody",
		Draft: true, Reviewers: []string{"a", "org/team"}, Assignees: []string{"me"}, Labels: []string{"x", "y"}, Milestone: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://github.com/acme/rocket/pull/7" || res.Number != 7 || res.Via != "gh" {
		t.Errorf("result %+v", res)
	}
	want := "gh pr create --repo github.com/acme/rocket --head me:feat --title T --body-file - --base main --draft --reviewer a,org/team --assignee me --label x,y --milestone v1"
	if got := tools.calls[len(tools.calls)-1]; got != want {
		t.Errorf("ran\n  %s\nwant\n  %s", got, want)
	}
	if got := tools.stdin[len(tools.stdin)-1]; got != "B\nody" {
		t.Errorf("body on stdin = %q", got)
	}
}

func TestGitHubCLIExisting(t *testing.T) {
	t.Parallel()
	tools := &fakeTools{
		answers: map[string]struct{ out, errOut string }{
			"gh auth status": {},
			"gh pr create":   {errOut: "a pull request for branch \"feat\" into branch \"main\" already exists:\nhttps://github.com/me/rocket/pull/3\n"},
		},
		failing: map[string]bool{"gh pr create": true},
	}
	u, _ := git.ParseRemoteURL("git@github.com:me/rocket.git")
	p, _ := For(t.Context(), u, nil, "auto", tools.env())
	res, err := p.CreatePR(t.Context(), Request{Remote: u, Head: "feat", Title: "T"})
	if err != nil || !res.Existing || res.URL != "https://github.com/me/rocket/pull/3" {
		t.Errorf("existing PR: %+v, %v", res, err)
	}
}

func TestGitLabCLI(t *testing.T) {
	t.Parallel()
	tools := &fakeTools{answers: map[string]struct{ out, errOut string }{
		"glab auth status": {},
		"glab mr create":   {out: "!12 Fix it\n https://gitlab.com/g/s/p/-/merge_requests/12\n"},
	}}
	u, _ := git.ParseRemoteURL("git@gitlab.com:g/s/p.git")
	p, _ := For(t.Context(), u, nil, "auto", tools.env())
	res, err := p.CreatePR(t.Context(), Request{Remote: u, Head: "fix", Base: "main", Title: "Fix it", Body: "desc", Reviewers: []string{"r"}, Labels: []string{"bug"}, Draft: true})
	if err != nil || res.URL != "https://gitlab.com/g/s/p/-/merge_requests/12" || res.Number != 12 {
		t.Fatalf("result %+v, %v", res, err)
	}
	want := "glab mr create --yes --repo https://gitlab.com/g/s/p --source-branch fix --title Fix it --description desc --target-branch main --draft --reviewer r --label bug"
	if got := tools.calls[len(tools.calls)-1]; got != want {
		t.Errorf("ran\n  %s\nwant\n  %s", got, want)
	}
}

func TestGitLabAPI(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var created map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer glpat-x" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.EscapedPath() == "/api/v4/projects/g%2Fs%2Fp" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"default_branch":"trunk"}`)
		case r.URL.Path == "/api/v4/users" && r.URL.Query().Get("username") == "rev":
			_, _ = io.WriteString(w, `[{"id":31}]`)
		case r.URL.Path == "/api/v4/users":
			_, _ = io.WriteString(w, `[]`)
		case r.URL.EscapedPath() == "/api/v4/projects/g%2Fs%2Fp/merge_requests" && r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &created)
			_, _ = io.WriteString(w, `{"iid":9,"web_url":"https://gl.example/g/s/p/-/merge_requests/9"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	tools := &fakeTools{answers: map[string]struct{ out, errOut string }{}}
	env := tools.env()
	env.Getenv = func(k string) string {
		if k == "GITLAB_TOKEN" {
			return "glpat-x"
		}
		return ""
	}
	u, _ := git.ParseRemoteURL("https://gl.example/g/s/p.git")
	hosts := []config.Host{{Host: "gl.example", Type: "gitlab", API: srv.URL + "/api/v4"}}
	p, err := For(t.Context(), u, hosts, "auto", env)
	if err != nil {
		t.Fatal(err)
	}
	if c := p.Check(t.Context(), ""); !c.OK || c.Via != "api" || !strings.Contains(c.Detail, "$GITLAB_TOKEN") {
		t.Errorf("Check = %+v", c)
	}
	res, err := p.CreatePR(t.Context(), Request{Remote: u, Head: "fix", Title: "Fix", Body: "b", Draft: true, Reviewers: []string{"rev", "ghost"}, Labels: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://gl.example/g/s/p/-/merge_requests/9" || res.Number != 9 || len(res.Warnings) != 1 {
		t.Errorf("result %+v", res)
	}
	if created["target_branch"] != "trunk" || created["title"] != "Draft: Fix" || created["labels"] != "a,b" {
		t.Errorf("created %v", created)
	}
	if ids, _ := created["reviewer_ids"].([]any); len(ids) != 1 || ids[0] != float64(31) {
		t.Errorf("reviewer_ids = %v", created["reviewer_ids"])
	}
}

func TestNoAuth(t *testing.T) {
	t.Parallel()
	tools := &fakeTools{answers: map[string]struct{ out, errOut string }{}}
	u, _ := git.ParseRemoteURL("git@github.com:a/b.git")
	p, _ := For(t.Context(), u, nil, "auto", tools.env())
	if _, err := p.CreatePR(t.Context(), Request{Remote: u, Head: "x", Title: "t"}); !errors.Is(err, ErrNoAuth) {
		t.Errorf("CreatePR without auth: %v", err)
	}
	if c := p.Check(t.Context(), ""); c.OK || !strings.Contains(c.Detail, "git credential helper") {
		t.Errorf("Check without auth = %+v", c)
	}
	if _, err := For(t.Context(), u, nil, "cli", tools.env()); err != nil {
		t.Fatal(err)
	}
	p, _ = For(t.Context(), u, nil, "cli", tools.env())
	if _, err := p.CreatePR(t.Context(), Request{Remote: u, Head: "x", Title: "t"}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("method cli without gh: %v", err)
	}
}

func TestTokenFromCredentialHelper(t *testing.T) {
	t.Parallel()
	tools := &fakeTools{answers: map[string]struct{ out, errOut string }{
		"git credential fill": {out: "protocol=https\nhost=github.com\nusername=me\npassword=gho_secret\n"},
	}}
	tok, _ := findToken(t.Context(), tools.env(), "github", config.Host{Host: "github.com"}, "")
	if tok.value != "gho_secret" || tok.source != "git credential helper" {
		t.Errorf("token %q from %q", tok.value, tok.source)
	}
	if got := tools.stdin[len(tools.stdin)-1]; got != "protocol=https\nhost=github.com\n\n" {
		t.Errorf("credential request %q", got)
	}
}

func TestAPIMessage(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		`{"message":"Validation Failed","errors":[{"message":"A pull request already exists for a:b."}]}`: "Validation Failed; A pull request already exists for a:b.",
		`{"message":["Another open merge request already exists"]}`:                                       "Another open merge request already exists",
		`{"message":{"base":["is invalid"]}}`:                                                             "base is invalid",
		"<html>\x1b[31mnope</html>":                                                                       "<html>nope</html>",
	}
	for in, want := range cases {
		if got := apiMessage([]byte(in)); got != want {
			t.Errorf("apiMessage(%q) = %q, want %q", in, got, want)
		}
	}
}
