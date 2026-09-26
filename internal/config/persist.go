package config

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// ContainerHome is the agent user's home inside the container.
const ContainerHome = "/home/agent"

// Persist is one entry of config.toml's [persist] table: a directory under
// the container's home that outlives the container, kept in the data dir
// under its name (datadir.PersistDir) and bind-mounted back at Container.
type Persist struct {
	Name string
	// Path is the entry as written, ~/<Rel>.
	Path string
	// Rel is the directory relative to the home, cleaned: .foo, .config/foo.
	Rel string
	// Container is where it is mounted: ContainerHome/Rel.
	Container string
}

// persistKey is config.toml's table of directories to persist by name.
const persistKey = "persist"

// PersistParents are the directories a persisted one may sit directly in:
// the home itself, and the ones the layer creates owned by the agent user
// (layer-user.sh). Docker creates a mountpoint's missing parents as root, so
// an entry deeper than these would leave a root-owned directory in the home
// that every tool keeping files next to the entry would fail to write.
var PersistParents = []string{"", ".config", ".local", ".local/share", ".cache"}

// ReservedHome are the home-relative paths caboose mounts itself, or keeps
// out of the data dir on purpose (.local/state holds per-boot locks). An
// entry may be none of them, inside none of them, and contain none of them.
// It must track the launcher's mounts (internal/launcher/container.go).
var ReservedHome = []string{
	".claude", ".claude.json",
	".local/bin", ".local/share/claude", ".local/state", ".cache/claude",
	".config/git", ".config/jj", ".config/gh",
	".ssh", ".caboose-sync",
}

// ParsePersist validates one [persist] entry, written as ~/<dir>.
func ParsePersist(name, p string) (Persist, error) {
	if !ValidRootName(name) {
		return Persist{}, fmt.Errorf("'%s' is not a persist name: lowercase letters, digits, - and _, starting with a letter or digit, at most 32", name)
	}
	rel, ok := strings.CutPrefix(p, "~/")
	if !ok {
		return Persist{}, fmt.Errorf("persist.%s must be a directory under the home, written ~/<dir>, not %q", name, p)
	}
	rel = strings.TrimSuffix(rel, "/")
	if rel == "" || rel == "." || rel == ".." || path.Clean(rel) != rel ||
		strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "../") {
		return Persist{}, fmt.Errorf("persist.%s: %q is not a plain directory under the home (no ., .., // or the home itself)", name, p)
	}
	if !contains(PersistParents, parentOf(rel)) {
		return Persist{}, fmt.Errorf("persist.%s: %q is too deep: it must sit directly in ~, ~/.config, ~/.local, ~/.local/share or ~/.cache "+
			"(docker would create the directories above it as root)", name, p)
	}
	for _, r := range ReservedHome {
		switch {
		case rel == r:
			return Persist{}, fmt.Errorf("persist.%s: ~/%s is caboose's own, persisted or kept out of the data dir already", name, r)
		case strings.HasPrefix(r, rel+"/"):
			return Persist{}, fmt.Errorf("persist.%s: ~/%s holds ~/%s, which caboose mounts itself; name the directory inside it you mean", name, rel, r)
		case strings.HasPrefix(rel, r+"/"):
			return Persist{}, fmt.Errorf("persist.%s: ~/%s is inside ~/%s, which caboose mounts itself", name, rel, r)
		}
	}
	return Persist{Name: name, Path: p, Rel: rel, Container: ContainerHome + "/" + rel}, nil
}

// parentOf is rel's parent directory, "" for one directly in the home.
func parentOf(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// readPersist reads the [persist] table: each entry by name, checked, in
// name order. Two entries may not name the same directory.
func readPersist(file string, v any) ([]Persist, error) {
	table, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: persist must be a table, [persist], of name = \"~/dir\"", file)
	}
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []Persist
	seen := map[string]string{}
	for _, name := range names {
		s, ok := table[name].(string)
		if !ok {
			return nil, fmt.Errorf("%s: persist.%s must be a directory, written ~/<dir>", file, name)
		}
		p, err := ParsePersist(name, s)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", file, err)
		}
		if other, dup := seen[p.Rel]; dup {
			return nil, fmt.Errorf("%s: persist.%s and persist.%s both name ~/%s; keep one", file, other, name, p.Rel)
		}
		seen[p.Rel] = name
		out = append(out, p)
	}
	return out, nil
}

// SamePersist reports whether a and b mount the same names at the same
// container paths, in any order.
func SamePersist(a, b []Persist) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[[2]string]bool{}
	for _, p := range a {
		m[[2]string{p.Name, p.Container}] = true
	}
	for _, p := range b {
		if !m[[2]string{p.Name, p.Container}] {
			return false
		}
	}
	return true
}

// DescribePersist is entries for a message: "~/.foo (foo), ~/.aws (aws)",
// or "none".
func DescribePersist(ps []Persist) string {
	if len(ps) == 0 {
		return "none"
	}
	var parts []string
	for _, p := range ps {
		parts = append(parts, "~/"+p.Rel+" ("+p.Name+")")
	}
	return strings.Join(parts, ", ")
}
