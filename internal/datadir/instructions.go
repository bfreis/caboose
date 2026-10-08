package datadir

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/nofollow"
)

// Facts is what a launch knows about the sandbox it installs the
// instructions for: the values behind the placeholders of sandbox/CLAUDE.md
// and the skills, and what their @@IF conditions test.
type Facts struct {
	Env          string        // environment name, config.DefaultEnv for the default
	Isolation    string        // container, gvisor or vm: the running sandbox's, else the configured one
	Profile      string        // isolation profile, "<kind>.<name>" or the bare kind
	Image        string        // image kind: apko, dockerfile or ref
	ImageProfile string        // image profile, "<kind>.<name>"
	Version      string        // the launcher's version
	Checkout     string        // host path of the checkout the launcher came from, or ""
	Roots        []config.Root // what the sandbox mounts
	HostExec     bool

	resolved   bool
	sourceKind string
	source     string
}

// Where caboose's source is, as @@IF source=...@@ tests it.
const (
	SourceCheckout = "checkout" // the launcher's checkout, under a root
	SourceClone    = "clone"    // a clone found under a root; the launcher is an installed release
	SourceOutside  = "outside"  // the launcher's checkout, outside every root
	SourceRelease  = "release"  // no checkout and no clone: upstream
)

// Resolve works out where the source is (FindClone reads the roots), so
// that expanding many files does it once. Expand does it when it has not
// been.
func (f Facts) Resolve() Facts {
	if f.resolved {
		return f
	}
	f.resolved = true
	if f.Checkout != "" {
		if p, ok := config.ContainerPath(f.Roots, f.Checkout); ok {
			f.sourceKind, f.source = SourceCheckout, p
		} else {
			f.sourceKind, f.source = SourceOutside, f.Checkout
		}
		return f
	}
	if clone := FindClone(f.Roots); clone != "" {
		if p, ok := config.ContainerPath(f.Roots, clone); ok {
			f.sourceKind, f.source = SourceClone, p
			return f
		}
	}
	f.sourceKind, f.source = SourceRelease, UpstreamURL
	return f
}

func (f Facts) env() string { return or(f.Env, config.DefaultEnv) }

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// values are the placeholders and what replaces them.
func (f Facts) values() []string {
	flag := ""
	if f.env() != config.DefaultEnv {
		flag = " -e " + f.env()
	}
	return []string{
		"@@CABOOSE_ROOTS@@", DescribeMounts(f.Roots),
		"@@CABOOSE_ENV@@", f.env(),
		"@@CABOOSE_ENV_FLAG@@", flag,
		"@@CABOOSE_ISOLATION@@", f.Isolation,
		"@@CABOOSE_PROFILE@@", f.Profile,
		"@@CABOOSE_IMAGE@@", f.ImageProfile,
		"@@CABOOSE_VERSION@@", f.Version,
		"@@CABOOSE_SOURCE@@", f.source,
		"@@CABOOSE_UPSTREAM@@", UpstreamURL,
	}
}

// condKeys are the keys of an @@IF condition and the values each takes
// (nil: any name, for env).
var condKeys = map[string][]string{
	"isolation": {"container", "gvisor", "vm"},
	"image":     {"apko", "dockerfile", "ref"},
	"source":    {SourceCheckout, SourceClone, SourceOutside, SourceRelease},
	"hostexec":  {"on", "off"},
	"env":       nil,
}

// current is the value key has for f.
func (f Facts) current(key string) string {
	switch key {
	case "isolation":
		return f.Isolation
	case "image":
		return f.Image
	case "source":
		return f.sourceKind
	case "hostexec":
		if f.HostExec {
			return "on"
		}
		return "off"
	}
	return f.env()
}

var (
	ifLine = regexp.MustCompile(`^@@IF ([a-z]+)(!?=)([A-Za-z0-9_.,-]+)@@$`)
)

// frame is one open @@IF.
type frame struct {
	parent, cond bool // the enclosing text is live; the condition holds
	inElse       bool
}

func (fr frame) live() bool { return fr.parent && (fr.cond != fr.inElse) }

// cond evaluates `key=v1,v2` or `key!=v1,v2`, an error for what is not one.
func (f Facts) cond(m []string) (bool, error) {
	key, op, vals := m[1], m[2], strings.Split(m[3], ",")
	allowed, ok := condKeys[key]
	if !ok {
		return false, fmt.Errorf("unknown condition key %q", key)
	}
	for _, v := range vals {
		if v == "" || (allowed != nil && !slices.Contains(allowed, v)) {
			return false, fmt.Errorf("unknown value %q for %s", v, key)
		}
	}
	has := slices.Contains(vals, f.current(key))
	return has == (op == "="), nil
}

// Expand fills in a file of sandbox/: its @@IF cond@@ / @@ELSE@@ / @@END@@
// blocks, each directive alone on its line (leading and trailing space
// allowed), and then its @@CABOOSE_...@@ placeholders. A condition is
// key=v1,v2 or key!=v1,v2, over the keys of condKeys: isolation, image,
// source, hostexec and env (where "default" is config.DefaultEnv). Blocks
// nest; a directive's line is removed whole, and where it or a removed
// block leaves more than one blank line they are cut to one.
//
// A malformed or unbalanced directive is an error, and so is anything left
// that looks like a placeholder or a directive, which this launcher does
// not know how to fill: that is checked before the values go in, so what
// they hold (a path the sandbox chose, say) is never taken for one.
//
// Directives are read everywhere, inside code fences too, and nothing
// escapes an @@: a template cannot show the syntax, or a literal @@NAME@@.
func Expand(src []byte, f Facts) ([]byte, error) {
	f = f.Resolve()
	var lines []string
	removed := map[int]bool{} // positions in lines where something was removed
	var stack []frame
	live := func() bool { return len(stack) == 0 || stack[len(stack)-1].live() }
	for n, line := range strings.SplitAfter(string(src), "\n") {
		t := strings.TrimSpace(line)
		fail := func(err error) ([]byte, error) { return nil, fmt.Errorf("line %d: %v", n+1, err) }
		switch {
		case strings.HasPrefix(t, "@@IF"):
			m := ifLine.FindStringSubmatch(t)
			if m == nil {
				return fail(fmt.Errorf("malformed directive %q", t))
			}
			c, err := f.cond(m)
			if err != nil {
				return fail(err)
			}
			stack = append(stack, frame{parent: live(), cond: c})
			removed[len(lines)] = true
		case t == "@@ELSE@@":
			if len(stack) == 0 || stack[len(stack)-1].inElse {
				return fail(errors.New("@@ELSE@@ without @@IF"))
			}
			stack[len(stack)-1].inElse = true
			removed[len(lines)] = true
		case t == "@@END@@":
			if len(stack) == 0 {
				return fail(errors.New("@@END@@ without @@IF"))
			}
			stack = stack[:len(stack)-1]
			removed[len(lines)] = true
		case live():
			lines = append(lines, line)
		default:
			removed[len(lines)] = true
		}
	}
	if len(stack) > 0 {
		return nil, errors.New("@@IF without @@END@@")
	}
	text := collapseBlanks(lines, removed)
	known := map[string]bool{}
	vals := f.values()
	for i := 0; i < len(vals); i += 2 {
		known[vals[i]] = true
	}
	var unknown []string
	for _, u := range UnknownPlaceholders([]byte(text)) {
		if !known[u] {
			unknown = append(unknown, u)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("uses placeholders this launcher doesn't know (%s)", strings.Join(unknown, ", "))
	}
	return []byte(strings.NewReplacer(vals...).Replace(text)), nil
}

// collapseBlanks joins lines, cutting to one each run of blank lines that
// touches a position where something was removed (removed[i]: between
// lines i-1 and i). Runs the author wrote are left alone.
func collapseBlanks(lines []string, removed map[int]bool) string {
	var b strings.Builder
	for i := 0; i < len(lines); {
		if strings.TrimSpace(lines[i]) != "" {
			b.WriteString(lines[i])
			i++
			continue
		}
		j := i
		for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
			j++
		}
		touched := false
		for k := i; k <= j; k++ {
			touched = touched || removed[k]
		}
		if !touched {
			b.WriteString(strings.Join(lines[i:j], ""))
			i = j
			continue
		}
		b.WriteString(lines[i])
		i = j
	}
	return b.String()
}

// placeholderPattern matches anything shaped like a placeholder or a
// directive.
var placeholderPattern = regexp.MustCompile(`@@[A-Z][A-Z0-9_]*@@|@@IF[^@\n]*@@`)

// UnknownPlaceholders lists what still looks like a placeholder or a
// directive in Expand's output, i.e. what this launcher does not know how
// to fill. A checkout's files can be newer than the launcher built from
// it -- pulled, but not rebuilt -- and a placeholder added since would
// otherwise be installed literally.
func UnknownPlaceholders(expanded []byte) []string {
	var names []string
	seen := map[string]bool{}
	for _, m := range placeholderPattern.FindAll(expanded, -1) {
		if !seen[string(m)] {
			seen[string(m)] = true
			names = append(names, string(m))
		}
	}
	return names
}

// ExpandTree expands a set of files, by path relative to ManagedDir
// ("CLAUDE.md", ".claude/skills/NAME/SKILL.md"): Markdown through Expand,
// anything else as it is. A file that does not expand, or leaves a
// placeholder this launcher cannot fill, is an error naming it.
func ExpandTree(files map[string][]byte, f Facts) (map[string][]byte, error) {
	f = f.Resolve()
	out := make(map[string][]byte, len(files))
	for _, name := range sortedKeys(files) {
		if path.Ext(name) != ".md" {
			out[name] = files[name]
			continue
		}
		b, err := Expand(files[name], f)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		out[name] = b
	}
	return out, nil
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// InstallManaged installs files (by path relative to ManagedDir, as
// ExpandTree returns them) into dataDir/ManagedDir, and reports whether it
// had to change anything. Under ManagedSkills it also removes what files
// does not have, and the directories that leaves empty, so a skill that is
// removed or renamed goes from the sandbox too.
//
// The data dir sits outside any checkout, so what the sandbox reads is
// tracked under sandbox/ and copied in from here. The launcher is the only
// part of this that sees both the checkout and the data dir, and running it
// per launch rather than per container start means an edit to a tracked
// file reaches the next session without a caboose restart.
//
// Written in place and only when it changed: the destination is inside a
// directory mount, so the inode trap of a single-file mount does not apply
// here -- but a rewrite in place is still the one that cannot surprise a
// live session.
//
// The sandbox has ManagedDir read-only, so nothing in it is the
// container's; it is still reached through internal/nofollow, as every
// file of the data dir a launch writes is, so that a link found there
// (nofollow.ErrNotPlain) is left alone rather than written through.
func InstallManaged(files map[string][]byte, dataDir string) (bool, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, ManagedDir), 0o755); err != nil {
		return false, err
	}
	d := nofollow.Dir(dataDir)
	changed := false
	want := map[string]bool{}
	wantDirs := map[string]bool{}
	for name := range files {
		rel := path.Join(ManagedDir, name)
		want[rel] = true
		for p := path.Dir(rel); p != "."; p = path.Dir(p) {
			wantDirs[p] = true
		}
	}
	// Prune first: what the set does not have, and what is of the other
	// type than the set has it (a directory where a file goes, a file where
	// a directory does, a link where either does), so a swap is no error. The sandbox cannot write
	// here, so nothing under ManagedSkills is anyone's but caboose's.
	var stale, dirs []string
	err := d.Walk(ManagedSkills, func(rel string, e fs.DirEntry) error {
		switch {
		case e.IsDir() && (!wantDirs[rel] || want[rel]):
			dirs = append(dirs, rel)
		case !e.IsDir() && (!want[rel] || wantDirs[rel] || !e.Type().IsRegular()):
			stale = append(stale, rel)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	// Files first, then directories deepest first (a stale directory holds
	// only stale entries, so each is empty by its turn).
	for _, rel := range stale {
		if err := d.Remove(rel); err != nil {
			return changed, err
		}
		changed = true
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := d.Remove(dirs[i]); err != nil {
			return changed, err
		}
		changed = true
	}
	for _, name := range sortedKeys(files) {
		rel := path.Join(ManagedDir, name)
		have, _, err := d.ReadFile(rel)
		switch {
		case err == nil && bytes.Equal(have, files[name]):
			continue
		case err == nil:
			err = d.WriteInPlace(rel, files[name])
		case errors.Is(err, fs.ErrNotExist):
			err = d.WriteFile(rel, files[name], 0o644)
		}
		if err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}
