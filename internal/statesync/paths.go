// Package statesync backs up and syncs the portable part of the data dir --
// what the sandbox config's rules name: memories, settings, skills,
// start-up scripts and shell config by default -- through a git remote,
// across machines.
//
// The data dir itself is never a git work tree: a checkout of a commit that
// predates an ignore rule lets a VCS snapshot live state and then delete it.
// So the sync repo is a separate directory
// (<data dir>/sync), and a sync goes export -> commit -> fetch -> merge ->
// apply: the files the rules name are copied into the repo, the merge
// happens there, and only what the merge changed is written back, file by
// file. Git never checks anything out over live state.
//
// What syncs is what the sandbox config's rules say (internal/sandboxcfg),
// on this machine: a path a remote sends that no rule here names is never
// written, and a file no rule names never leaves. So something new
// appearing in ~/.claude (a credential, a cache) does not start syncing on
// its own, and a remote cannot make it.
package statesync

import (
	"fmt"
	"path"
	"strings"

	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/sandboxcfg"
)

// RepoHome is the sync repo's directory for the home: a file synced from
// ~/<rel> is RepoHome+<rel> there, the same on every machine.
const RepoHome = "home/"

// ProjectKey is the directory name Claude Code keeps a project's state
// under, for a cwd (sandboxcfg.ProjectKey).
func ProjectKey(cwd string) string { return sandboxcfg.ProjectKey(cwd) }

// Target is where a repo path lives on this machine, and how it syncs.
type Target struct {
	// Rel is the path relative to the data dir, slash-separated.
	Rel string
	// Home is the path relative to the home.
	Home string
	// Rule is the sandbox config's rule that syncs it.
	Rule *sandboxcfg.Rule
	// Stop is the data dir path deleting files never prunes: the
	// directory its rule syncs (up to the first glob), or its keep
	// entry's, which must survive being emptied.
	Stop string
	// InPlace is set for a keep entry that is a file: a single-file bind
	// mount, which is written in place, never replaced, and never
	// deleted.
	InPlace bool
}

// Keys reports whether only some of the file's keys sync.
func (t Target) Keys() bool { return t.Rule.Merge == sandboxcfg.MergeJSON && len(t.Rule.Keys) > 0 }

// LiveTarget maps a repo path to its place in the data dir, or returns an
// error for a path no rule of c syncs, or one that would escape.
func LiveTarget(c *sandboxcfg.Config, repoPath string) (Target, error) {
	if !safeRel(repoPath) {
		return Target{}, fmt.Errorf("%q: not a clean relative path", repoPath)
	}
	rel, ok := strings.CutPrefix(repoPath, RepoHome)
	if !ok || rel == "" {
		return Target{}, fmt.Errorf("%q: not a synced path", repoPath)
	}
	r, ok := c.Match(rel)
	if !ok {
		return Target{}, fmt.Errorf("%q: not synced by this machine's sandbox config", repoPath)
	}
	k, _ := c.Keeps(rel)
	stop := r.Base()
	if stop == rel {
		stop = path.Dir(rel)
	}
	if stop != k.Rel && !strings.HasPrefix(stop, k.Rel+"/") {
		stop = k.Rel
	}
	return Target{Rel: datadir.Home(rel), Home: rel, Rule: r, Stop: datadir.Home(stop), InPlace: k.File}, nil
}

// safeRel reports whether p is a slash-separated relative path with no
// empty, "." or ".." element and nothing git would treat specially.
func safeRel(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	for _, el := range strings.Split(p, "/") {
		if el == "." || el == ".." || strings.EqualFold(el, ".git") {
			return false
		}
	}
	return true
}
