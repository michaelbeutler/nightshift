// Package version reports which build of nightshift is running.
package version

import (
	"runtime/debug"
	"strings"
)

// These are set by the release build through -ldflags "-X ...". A plain
// `go install` leaves them empty and String falls back to the module version
// and VCS stamp that the Go toolchain records in the binary.
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

// String returns a one-line description such as "v0.1.0 (abc1234, 2026-11-24)".
func String() string {
	v, c, d := Version, Commit, Date
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
				}
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			}
		}
	}
	if v == "" {
		v = "dev"
	}
	var extra []string
	if len(c) > 7 {
		c = c[:7]
	}
	if c != "" {
		extra = append(extra, c)
	}
	if len(d) >= 10 {
		extra = append(extra, d[:10])
	}
	if len(extra) == 0 {
		return v
	}
	return v + " (" + strings.Join(extra, ", ") + ")"
}

// Short returns just the version, for User-Agent headers and the MCP handshake.
func Short() string {
	s := String()
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}
