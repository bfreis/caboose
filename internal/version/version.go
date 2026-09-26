// Package version says which build of the launcher is running.
//
// A release binary is stamped by goreleaser, and a `make launcher` build by
// the Makefile, with
//
//	-ldflags "-X github.com/bfreis/caboose/internal/version.Version=v1.2.3
//	          -X github.com/bfreis/caboose/internal/version.Commit=<sha>
//	          -X github.com/bfreis/caboose/internal/version.Date=<rfc3339>"
//
// Anything else -- `go install ...@v1.2.3`, a bare `go build` in a checkout --
// carries no ldflags, but the Go toolchain records the module version and the
// VCS state in the binary, and that is enough to tell builds apart.
package version

import "runtime/debug"

// Set with -X at link time; empty in an unstamped build. The names are part
// of the release tooling's contract, so do not rename them.
var (
	Version string
	Commit  string
	Date    string
)

// Info describes a build. Commit, Date and Modified may be unknown (empty,
// false); Version never is, falling back to "dev".
type Info struct {
	Version  string
	Commit   string
	Date     string
	Modified bool // built from a working tree with uncommitted changes
}

// Get returns the running binary's build info: the link-time stamp when
// there is one, otherwise whatever the toolchain recorded.
func Get() Info {
	bi, _ := debug.ReadBuildInfo()
	return resolve(Version, Commit, Date, bi)
}

// resolve is Get over explicit inputs, so every fallback can be tested.
//
// A stamped Version wins outright, together with whatever Commit and Date
// were stamped alongside it: mixing in the toolchain's VCS settings would
// describe the tree goreleaser built in, not the release. Unstamped, the
// module version comes first: the tag `go install` fetched, or, for a
// `go build` in a checkout, the version Go derives from the VCS (a tag, or a
// pseudo-version like v0.0.0-20260921055357-3fea36d9fc96+dirty). It is
// "(devel)" only when there was no VCS to ask (-buildvcs=false, no git).
func resolve(version, commit, date string, bi *debug.BuildInfo) Info {
	if version != "" {
		return Info{Version: version, Commit: commit, Date: date}
	}
	info := Info{Version: "dev"}
	if bi == nil {
		return info
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		info.Version = v
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Commit = s.Value
		case "vcs.time":
			info.Date = s.Value
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	return info
}
