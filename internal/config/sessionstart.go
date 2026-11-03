package config

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
)

// startKey is [session]'s sub-table of start directories,
// [session.start].
const startKey = "start"

// StartDir maps a host project directory to where its sessions start in the
// sandbox: Host is absolute (~ expanded) and clean, Target an absolute,
// clean sandbox path.
type StartDir struct {
	Host, Target string
}

// CheckStartDirs checks [session.start] as written (host directory by
// start directory) and returns it sorted by host directory, ~ in a key
// expanded with home ("" leaves it as it is). A key is an absolute path or
// one starting with ~/, an empty or relative one is refused, and so are two
// keys that name the same path once cleaned. A value is an absolute, clean
// path with no control or invisible character. It also holds no # : tmux
// expands formats such as #{session_name} in the directory it is given.
// Any absolute path may be a start directory, since it is only a working
// directory: it may be in the image or the home.
func CheckStartDirs(m map[string]string, home string) ([]StartDir, error) {
	var out []StartDir
	seen := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		fail := func(format string, args ...any) error {
			return fmt.Errorf("[session.start] %q: "+format, append([]any{k}, args...)...)
		}
		if k == "" {
			return nil, fmt.Errorf("[session.start] has an empty host directory")
		}
		if !printableText(k) {
			return nil, fail("the host directory has a control or invisible character")
		}
		host := expandTilde(k, home)
		if !path.IsAbs(host) && host != "~" && !strings.HasPrefix(host, "~/") {
			return nil, fail("the host directory is not an absolute path (or one starting with ~/)")
		}
		if host == "~" {
			return nil, fail("the host directory is the home, which has no ~ without a home; name it in full")
		}
		host = path.Clean(host)
		if prev, dup := seen[host]; dup {
			return nil, fmt.Errorf("[session.start] %q and %q name the same directory (%s)", prev, k, host)
		}
		seen[host] = k
		v := m[k]
		switch {
		case v == "":
			return nil, fail("the start directory is empty")
		case !printableText(v):
			return nil, fail("the start directory %q has a control or invisible character", v)
		case strings.Contains(v, "#"):
			return nil, fail("the start directory %q has a #, which tmux would read as the start of a format; use a path without one", v)
		case !path.IsAbs(v):
			return nil, fail("the start directory %q is not an absolute sandbox path (~ is not expanded there)", v)
		case path.Clean(v) != v:
			return nil, fail("the start directory %q is not clean; write it as %q", v, path.Clean(v))
		}
		out = append(out, StartDir{Host: host, Target: v})
	}
	slices.SortFunc(out, func(a, b StartDir) int { return strings.Compare(a.Host, b.Host) })
	return out, nil
}

// readStartDirs reads the [session.start] table into the file's
// SessionStart, as written.
func (f *File) readStartDirs(v any) error {
	t, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf(`session.%s must be a table, [session.%s], of "host directory" = "sandbox directory"`, startKey, startKey)
	}
	m := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(t)) {
		s, ok := t[k].(string)
		if !ok {
			return fmt.Errorf("[session.start] %q must be a string, the directory in the sandbox", k)
		}
		m[k] = s
	}
	if _, err := CheckStartDirs(m, ""); err != nil {
		return err
	}
	if len(m) > 0 {
		f.SessionStart = m
	}
	return nil
}
