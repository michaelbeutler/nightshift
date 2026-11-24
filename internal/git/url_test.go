package git

import "testing"

func TestParseRemoteURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in                       string
		scheme, host, port, path string
	}{
		{"git@github.com:octo/rocket.git", "ssh", "github.com", "", "octo/rocket"},
		{"github.com:octo/rocket", "ssh", "github.com", "", "octo/rocket"},
		{"work-github:octo/rocket.git", "ssh", "work-github", "", "octo/rocket"},
		{"ssh://git@gitlab.example.com:2222/group/sub/project.git", "ssh", "gitlab.example.com", "2222", "group/sub/project"},
		{"git+ssh://git@github.com/octo/rocket", "ssh", "github.com", "", "octo/rocket"},
		{"https://github.com/octo/rocket.git", "https", "github.com", "", "octo/rocket"},
		{"https://oauth2:secret@gitlab.com/group/project/", "https", "gitlab.com", "", "group/project"},
		{"HTTPS://GitHub.com/Octo/Rocket", "https", "github.com", "", "Octo/Rocket"},
		{"git://example.org/repo.git", "git", "example.org", "", "repo"},
		{"/srv/git/repo.git", "file", "", "", "/srv/git/repo.git"},
		{"../remote.git", "file", "", "", "../remote.git"},
		{"file:///srv/git/repo.git", "file", "", "", "/srv/git/repo.git"},
		{`C:\repos\remote.git`, "file", "", "", `C:\repos\remote.git`},
	}
	for _, c := range cases {
		u, err := ParseRemoteURL(c.in)
		if err != nil {
			t.Errorf("ParseRemoteURL(%q): %v", c.in, err)
			continue
		}
		if u.Scheme != c.scheme || u.Host != c.host || u.Port != c.port || u.Path != c.path {
			t.Errorf("ParseRemoteURL(%q) = %+v, want scheme %q host %q port %q path %q", c.in, u, c.scheme, c.host, c.port, c.path)
		}
	}
	u, _ := ParseRemoteURL("https://oauth2:secret@gitlab.com/group/sub/project.git")
	if u.Owner() != "group" || u.Name() != "project" || u.String() != "gitlab.com/group/sub/project" {
		t.Errorf("Owner/Name/String = %q %q %q", u.Owner(), u.Name(), u.String())
	}
	if _, err := ParseRemoteURL("  "); err == nil {
		t.Error("an empty URL parsed")
	}
}

func TestParseIdent(t *testing.T) {
	t.Parallel()
	id, err := ParseIdent("Ada Lovelace <ada@example.com> 1764036000 +0100")
	if err != nil || id.Name != "Ada Lovelace" || id.Email != "ada@example.com" {
		t.Fatalf("ParseIdent = %+v, %v", id, err)
	}
	if d := identDate("Ada <ada@example.com> 1764036000 +0100"); d.Unix() != 1764036000 {
		t.Errorf("identDate = %v", d)
	}
	if _, err := ParseIdent("nobody"); err == nil {
		t.Error("an identity without an email parsed")
	}
}
