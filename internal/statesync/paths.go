// Package statesync backs up and syncs the portable part of the data dir --
// memories, settings, skills -- through a git remote, across machines.
//
// The data dir itself is never a git work tree: a checkout of a commit that
// predates an ignore rule lets a VCS snapshot live state and then delete it.
// So the sync repo is a separate directory
// (<data dir>/sync), and a sync goes export -> commit -> fetch -> merge ->
// apply: an allowlist of live files is copied into the repo, the merge
// happens there, and only what the merge changed is written back, file by
// file. Git never checks anything out over live state.
//
// It is an allowlist, not a denylist, for the reason the image's build
// context is one: something new appearing in ~/.claude (a credential, a
// cache) must not start syncing on its own.
package statesync

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Paths in the sync repo. Everything the repo holds is one of these; a path
// from a remote that is not is ignored, never written anywhere.
const (
	// repoClaude is the prefix for files synced out of ~/.claude.
	repoClaude = "claude/"
	// repoProjects holds each project's memory under its project key.
	repoProjects = repoClaude + "projects/"
	// repoSettings is ~/.claude/settings.json.
	repoSettings = repoClaude + "settings.json"
	// RepoClaudeJSON holds the ClaudeJSONKeys of ~/.claude.json, and only
	// those: the rest of that file (the OAuth account, caches, per-path
	// project trust) never leaves the machine.
	RepoClaudeJSON = "claude.json"
)

// TreeDirs are the directories of ~/.claude synced whole, file by file.
var TreeDirs = []string{"skills", "agents", "commands"}

// notSynced are paths inside TreeDirs that are not the user's: skills/synced
// is Claude Code's cache of the skills the account provides (docx, pdf,
// ...), downloaded on every machine by Claude Code itself. Syncing it would
// upload megabytes of files nobody wrote, and every update of the cache
// would read as a change -- and a conflict with the other machine's.
var notSynced = map[string]bool{"skills/synced": true}

// synced reports whether rel, a path under ~/.claude such as
// "skills/x/SKILL.md", is outside every notSynced prefix.
func synced(rel string) bool {
	for p := range notSynced {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return false
		}
	}
	return true
}

// ClaudeJSONKeys are the keys of ~/.claude.json that sync. User-scoped MCP
// servers are configuration someone sets up once and wants everywhere;
// nothing else in the file is.
var ClaudeJSONKeys = []string{"mcpServers"}

// maxKeyLen is the longest project key a remote may name: a directory
// name's limit on every filesystem that matters. Claude Code itself
// truncates a long cwd's key well below it, adding a hash.
const maxKeyLen = 255

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// ProjectKey is the directory name Claude Code keeps a project's state
// under, for a cwd: every character other than an ASCII letter or digit
// becomes '-'.
func ProjectKey(cwd string) string {
	return nonAlnum.ReplaceAllString(cwd, "-")
}

// workKey is the project key of /work, where the container mounts every
// repo root.
var workKey = ProjectKey("/work")

// SyncedKey reports whether project key k syncs: a project under /work,
// which is the same checkout on every machine -- the container mounts each
// repo root at the same path everywhere, so the key Claude Code derives
// from the cwd is the same too, and syncs as it is. Anything else (a
// session started in ~, or /tmp) is this machine's alone. It is also the
// check on a key a remote names, which must be one Claude Code could have
// written: it becomes a directory name here.
func SyncedKey(k string) bool {
	if k != workKey && !strings.HasPrefix(k, workKey+"-") {
		return false
	}
	return len(k) <= maxKeyLen && ProjectKey(k) == k
}

// Target is where a repo path lives on this machine.
type Target struct {
	// Rel is the path relative to the data dir, slash-separated; for
	// RepoClaudeJSON, the whole .claude.json the keys are merged into.
	Rel string
	// ClaudeJSON is set for RepoClaudeJSON, which is merged into Rel by key
	// rather than written over it.
	ClaudeJSON bool
}

// LiveTarget maps a repo path to its place in the data dir, or returns an
// error for a path outside the allowlist or one that would escape it.
func LiveTarget(repoPath string) (Target, error) {
	if !safeRel(repoPath) {
		return Target{}, fmt.Errorf("%q: not a clean relative path", repoPath)
	}
	switch {
	case repoPath == RepoClaudeJSON:
		return Target{Rel: ".claude.json", ClaudeJSON: true}, nil
	case repoPath == repoSettings:
		return Target{Rel: ".claude/settings.json"}, nil
	case strings.HasPrefix(repoPath, repoProjects):
		rest := strings.TrimPrefix(repoPath, repoProjects)
		key, file, ok := strings.Cut(rest, "/memory/")
		if !ok || strings.Contains(key, "/") || file == "" {
			return Target{}, fmt.Errorf("%q: not a project memory file", repoPath)
		}
		if !SyncedKey(key) {
			return Target{}, fmt.Errorf("%q: %q is not a synced project key", repoPath, key)
		}
		return Target{Rel: ".claude/projects/" + key + "/memory/" + file}, nil
	}
	for _, d := range TreeDirs {
		if file, ok := strings.CutPrefix(repoPath, repoClaude+d+"/"); ok && file != "" {
			if !synced(d + "/" + file) {
				return Target{}, fmt.Errorf("%q: not synced (Claude Code's own)", repoPath)
			}
			return Target{Rel: ".claude/" + d + "/" + file}, nil
		}
	}
	return Target{}, fmt.Errorf("%q: not a synced path", repoPath)
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
