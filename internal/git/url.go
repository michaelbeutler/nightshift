package git

import (
	"fmt"
	"net/url"
	"strings"
)

// RemoteURL is a parsed remote URL. Every form git accepts is understood:
// scp-like (git@host:owner/repo.git), ssh://, https://, git:// and local
// paths (which have no Host).
type RemoteURL struct {
	Raw    string `json:"-"`
	Scheme string `json:"scheme"` // ssh, https, http, git or file
	Host   string `json:"host,omitempty"`
	Port   string `json:"port,omitempty"`
	// Path is the repository path without leading slash or ".git", such as
	// "owner/repo" or "group/subgroup/project".
	Path string `json:"path"`
}

// ParseRemoteURL parses a remote URL. User information (a token embedded in
// an https URL, say) is dropped, never kept.
func ParseRemoteURL(raw string) (RemoteURL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return RemoteURL{}, fmt.Errorf("empty remote URL")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return RemoteURL{}, fmt.Errorf("remote URL: %w", err)
		}
		scheme := strings.ToLower(u.Scheme)
		switch scheme {
		case "git+ssh", "ssh+git":
			scheme = "ssh"
		}
		if scheme == "file" {
			return RemoteURL{Raw: raw, Scheme: "file", Path: u.Path}, nil
		}
		return RemoteURL{Raw: raw, Scheme: scheme, Host: strings.ToLower(u.Hostname()), Port: u.Port(), Path: cleanRepoPath(u.Path)}, nil
	}
	// scp-like syntax: [user@]host:path, recognized by a colon before any slash.
	if i := strings.IndexByte(raw, ':'); i > 0 && !strings.ContainsAny(raw[:i], `/\`) {
		host := raw[:i]
		if j := strings.LastIndexByte(host, '@'); j >= 0 {
			host = host[j+1:]
		}
		if len(host) > 1 { // "C:\repo" is a Windows path, not host "C"
			return RemoteURL{Raw: raw, Scheme: "ssh", Host: strings.ToLower(host), Path: cleanRepoPath(raw[i+1:])}, nil
		}
	}
	return RemoteURL{Raw: raw, Scheme: "file", Path: raw}, nil
}

func cleanRepoPath(p string) string {
	p = strings.Trim(p, "/")
	p = strings.TrimSuffix(p, ".git")
	return strings.Trim(p, "/")
}

// Owner returns the first path segment: the user, organization or top-level group.
func (u RemoteURL) Owner() string {
	owner, _, _ := strings.Cut(u.Path, "/")
	return owner
}

// Name returns the last path segment: the repository's name.
func (u RemoteURL) Name() string {
	return u.Path[strings.LastIndexByte(u.Path, '/')+1:]
}

// IsLocal reports whether the remote is a path on this machine.
func (u RemoteURL) IsLocal() bool { return u.Host == "" }

// String returns host/path, or the path for local remotes.
func (u RemoteURL) String() string {
	if u.IsLocal() {
		return u.Path
	}
	return u.Host + "/" + u.Path
}
