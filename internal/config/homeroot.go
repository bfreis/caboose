package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// HoldsDir reports whether the directory root is dir or holds it, judged
// by identity as well as by path: dir's nearest existing ancestor (dir
// itself when it exists) and every directory above it, by its path as
// given and by its physical path, are compared with root by device and
// inode, so a second name for root (a symlink, a bind mount, a firmlink,
// another case on a case-insensitive volume) is the same root. Where no
// part of dir can be looked at, its physical path is compared as a
// string. Both must be absolute.
func HoldsDir(root, dir string) bool {
	if root == "" || dir == "" || !filepath.IsAbs(root) || !filepath.IsAbs(dir) {
		return false
	}
	dir = filepath.Clean(dir)
	rootPhys := filepath.Clean(root)
	if p, err := filepath.EvalSymlinks(root); err == nil {
		rootPhys = p
	}
	rootFI, rootErr := os.Stat(root)
	// The nearest existing ancestor, and what of dir lies below it.
	exist, rest := dir, ""
	for {
		if _, err := os.Lstat(exist); err == nil {
			break
		}
		parent := filepath.Dir(exist)
		if parent == exist {
			break
		}
		rest = filepath.Join(filepath.Base(exist), rest)
		exist = parent
	}
	phys := exist
	if p, err := filepath.EvalSymlinks(exist); err == nil {
		phys = p
	}
	if Within(filepath.Join(phys, rest), rootPhys) || Within(dir, filepath.Clean(root)) {
		return true
	}
	if rootErr != nil {
		return false
	}
	for _, start := range []string{exist, phys} {
		for p := start; ; p = filepath.Dir(p) {
			if fi, err := os.Stat(p); err == nil && os.SameFile(fi, rootFI) {
				return true
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	return false
}

// narrowerNames are the subdirectories a narrower root is suggested from
// first, in this order, when the root has them.
var narrowerNames = []string{"dev", "src", "code", "projects", "work", "repos", "git", "Developer"}

// CabooseHomeRootProblem says why the root at host (physical) cannot be
// one when it holds CABOOSE_HOME (HoldsDir), naming a narrower root to use
// instead: a subdirectory of the root that does not hold it, shown with ~
// for home. "" when the root does not hold it.
func CabooseHomeRootProblem(host, cabooseHome, home string) string {
	if !HoldsDir(host, cabooseHome) {
		return ""
	}
	if HoldsDir(cabooseHome, host) {
		return "is CABOOSE_HOME (" + cabooseHome + "), where caboose keeps every environment's configuration, data dirs and checkpoints: a sandbox could change them; name the directory that holds your projects instead"
	}
	why := "holds CABOOSE_HOME (" + cabooseHome + "), where caboose keeps every environment's configuration, data dirs and checkpoints: a sandbox could change them"
	if alt := narrowerRoot(host, cabooseHome); alt != "" {
		return why + "; use " + tildeFor(alt, home) + " instead of " + tildeFor(host, home)
	}
	return why + "; name a directory under " + tildeFor(host, home) + " that holds your projects instead"
}

// narrowerRoot is a subdirectory of host that does not hold cabooseHome:
// one of narrowerNames if host has it, else the first by name; never a
// hidden one or ~/Library. "" for none.
func narrowerRoot(host, cabooseHome string) string {
	entries, err := os.ReadDir(host)
	if err != nil {
		return ""
	}
	var ok []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() || strings.HasPrefix(n, ".") || n == "Library" {
			continue
		}
		if p := filepath.Join(host, n); !HoldsDir(p, cabooseHome) && !HoldsDir(cabooseHome, p) {
			ok = append(ok, n)
		}
	}
	for _, n := range narrowerNames {
		if slices.Contains(ok, n) {
			return filepath.Join(host, n)
		}
	}
	if len(ok) == 0 {
		return ""
	}
	slices.Sort(ok)
	return filepath.Join(host, ok[0])
}

// tildeFor is p with home shown as ~.
func tildeFor(p, home string) string {
	if home == "" {
		return p
	}
	if hp, err := filepath.EvalSymlinks(home); err == nil {
		home = hp
	}
	switch {
	case p == home:
		return "~"
	case strings.HasPrefix(p, home+"/"):
		return "~/" + strings.TrimPrefix(p, home+"/")
	}
	return p
}
