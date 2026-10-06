// Package sandboxcfg is the sandbox config: ~/.config/caboose/sandbox.toml
// in the sandbox, which says what of the sandbox's home is kept across
// containers ([[keep]]) and what of that caboose sync carries between
// machines (sync, [[keep.sync]]).
//
// It is the sandbox's own file -- a session may edit it, and it syncs like
// any other -- so everything in it is untrusted: the host reads it through
// nofollow, and nothing in it can name a host path or an image. A bad entry
// is skipped and reported (Config.Problems), never the whole file: the file
// arrives by sync, and a mistake made on one machine must not stop a
// launch on another. Only a file that does not parse, or is of a newer
// format than this caboose reads, is refused whole (the launcher then uses
// the last one it could read).
package sandboxcfg

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

const (
	// FileName is the sandbox config's name, in ~/.config/caboose.
	FileName = "sandbox.toml"
	// Rel is where it is, relative to the home.
	Rel = ".config/caboose/" + FileName
	// HomePath is Rel as a person writes it.
	HomePath = "~/" + Rel

	// Format is the structure this caboose reads and writes. A file of a
	// newer one is refused whole (ErrNewerFormat); an older one is brought
	// up to it by Update.
	Format = 1
	// DefaultsVersion is the version of Default this caboose writes. A file
	// written from an older one is offered what was added since (Update).
	DefaultsVersion = 1

	// MaxSize is the largest sandbox config read.
	MaxSize = 256 << 10
)

// Merges are the ways a synced file merges.
const (
	// MergeText is git's three-way merge, line by line.
	MergeText = "text"
	// MergeJSON merges two JSON documents key by key (statesync.MergeJSON).
	MergeJSON = "json"
	// MergeUnion is a text merge that keeps the lines both sides added,
	// for a file that is a list of lines, such as an index.
	MergeUnion = "union"
)

var merges = []string{MergeText, MergeJSON, MergeUnion}

// ErrNewerFormat is Parse's error for a file of a newer format than Format.
var ErrNewerFormat = errors.New("newer format")

// Rule is one sync rule: what of a keep entry travels, and how it merges.
type Rule struct {
	// Path is the rule as written, ~/... with globs allowed.
	Path string
	// Merge is one of the Merge constants.
	Merge string
	// Keys, with MergeJSON, are the only top-level keys of each file that
	// sync; nil for all of them. The rest never leave the machine.
	Keys []string
	// Exclude are the patterns left out of what the rule matches, as
	// written.
	Exclude []string

	pattern  []string   // Path's components, relative to the home
	excludes [][]string // each exclude's components; one component for a bare name
	names    []bool     // whether each exclude is a bare name, matched at any depth
	roots    []string   // the project keys of the config's roots, for RootsComponent
}

// RootsComponent, as a whole component of a rule's path or exclude, stands
// for the directory Claude Code keeps the state of a project under one of
// the config's roots in: the root's ProjectKey, or that key, a '-', and
// more (a project below the root). It is resolved from the roots the file
// lists, never from this machine's, so a rule means the same on every
// machine the file syncs to, and a sync records it with the file.
const RootsComponent = "{roots}"

// ProjectKey is the name Claude Code gives the directory it keeps a
// project's state in, under ~/.claude/projects, for the project's path:
// every character other than an ASCII letter or digit becomes '-'. It is
// lossy: /work/a.b and /work/a/b are both -work-a-b.
func ProjectKey(p string) string {
	b := []byte(p)
	for i, c := range b {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// Keep is one [[keep]] entry: a directory (or a file) of the home kept in
// the data dir across containers, and the rules for what of it syncs.
type Keep struct {
	// Path is the entry as written, ~/<Rel>.
	Path string
	// Rel is the path relative to the home, cleaned.
	Rel string
	// File is set for a file entry (a single-file mount), rather than a
	// directory.
	File bool
	// Required is set for the entries caboose itself needs (Required),
	// kept whether or not the file lists them.
	Required bool
	// Listed is set when the file lists the entry; a Required one it does
	// not list is added, unsynced.
	Listed bool
	// Rules say what of it syncs, first match first; none when nothing
	// does.
	Rules []Rule
}

// Config is a sandbox config, checked.
type Config struct {
	// Format and Defaults are the file's, as written; a file that omits
	// them is taken as current.
	Format   int
	Defaults int
	// Roots are the paths in the sandbox of the roots the projects expect:
	// Claude Code keys a project's state by its path, so a machine that
	// mounts no root there never reads it.
	Roots []string
	// Keep are the entries in effect, in the file's order, then any
	// Required one it does not list.
	Keep []Keep
	// Problems are what was skipped, and why, one line each.
	Problems []string
}

// Required are the entries caboose needs whatever the file says: Claude
// Code's state and login, its settings file (a single-file mount), and
// ~/.config/caboose, which holds this file -- the list of entries has to
// be kept to be read at all.
var Required = []Keep{
	{Path: "~/.claude", Rel: ".claude", Required: true},
	{Path: "~/.claude.json", Rel: ".claude.json", File: true, Required: true},
	{Path: "~/.config/caboose", Rel: ".config/caboose", Required: true},
}

// Parents are the directories a keep entry may sit directly in: the home
// itself, and the ones the image's layer creates owned by the agent user
// (layer-user.sh). Docker creates a mount point's missing parents as root,
// so an entry deeper than these would leave a root-owned directory in the
// home that every tool keeping files next to it would fail to write.
var Parents = []string{"", ".config", ".local", ".local/share", ".cache"}

// Reserved are home-relative paths caboose mounts itself, or keeps out of
// the data dir on purpose (.local/state holds per-boot locks): no entry may
// be one of them, hold one, or sit inside one. They must track the
// launcher's own mounts (internal/launcher/container.go).
var Reserved = []string{
	".local/bin", ".local/share/claude", ".local/state", ".cache/claude",
	".caboose-sync", ".caboose-proposals",
}

// Denied are home-relative files that never sync, whatever a rule says:
// Claude Code's login.
var Denied = []string{".claude/.credentials.json"}

// Parse reads and checks a sandbox config. The error is for a file that
// cannot be used at all: not UTF-8, not TOML, too large, or of a newer
// format (ErrNewerFormat); everything else is a Problem.
func Parse(data []byte) (*Config, error) {
	if len(data) > MaxSize {
		return nil, fmt.Errorf("larger than %d KiB", MaxSize>>10)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("not UTF-8")
	}
	var raw map[string]any
	if _, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("not valid TOML: %v", err)
	}
	c := &Config{Format: Format, Defaults: DefaultsVersion}
	for _, k := range sortedKeys(raw) {
		v := raw[k]
		switch k {
		case "format":
			n, ok := v.(int64)
			if !ok || n < 1 {
				c.problem("format must be a whole number, 1 or more; taken as %d", Format)
				continue
			}
			if n > Format {
				return nil, fmt.Errorf("%w: it is format %d, and this caboose reads up to %d", ErrNewerFormat, n, Format)
			}
			c.Format = int(n)
		case "defaults":
			n, ok := v.(int64)
			if !ok || n < 0 {
				c.problem("defaults must be a whole number; taken as %d", DefaultsVersion)
				continue
			}
			c.Defaults = int(n)
		case "roots":
			c.Roots = c.parseRoots(v)
		case "keep":
			list, ok := v.([]map[string]any)
			if !ok {
				c.problem("keep must be a list of tables, [[keep]]")
				continue
			}
			for i, e := range list {
				if k, ok := c.parseKeep(i+1, e); ok {
					c.add(k)
				}
			}
		default:
			c.problem("unknown setting %q, ignored (a newer caboose may know it: caboose update)", k)
		}
	}
	for _, r := range Required {
		if c.find(r.Rel) < 0 {
			c.problem("%s is not listed, but caboose needs it: kept anyway, not synced", r.Path)
			c.add(r)
		}
	}
	// Keys come before roots in the file's order: the rules learn the
	// roots once both are read.
	var keys []string
	for _, r := range c.Roots {
		keys = append(keys, ProjectKey(r))
	}
	for i := range c.Keep {
		for j := range c.Keep[i].Rules {
			c.Keep[i].Rules[j].roots = keys
		}
	}
	return c, nil
}

// Default is the sandbox config caboose writes for roots, the container
// paths of the host config's roots.
func Default(roots []string) []byte {
	q := make([]string, len(roots))
	for i, r := range roots {
		q[i] = fmt.Sprintf("%q", r)
	}
	return []byte(strings.Replace(defaultText, "roots = []", "roots = ["+strings.Join(q, ", ")+"]", 1))
}

func (c *Config) problem(format string, args ...any) {
	c.Problems = append(c.Problems, fmt.Sprintf(format, args...))
}

// find is the index of the entry for rel, -1 when there is none.
func (c *Config) find(rel string) int {
	for i, k := range c.Keep {
		if k.Rel == rel {
			return i
		}
	}
	return -1
}

// add appends k, unless it overlaps an entry already in: then the later
// one is skipped -- or, when k is Required and the other is not, the other.
func (c *Config) add(k Keep) {
	for i, o := range c.Keep {
		if !overlaps(k.Rel, o.Rel) {
			continue
		}
		if k.Required && !o.Required {
			c.problem("%s overlaps %s, which caboose needs: %s skipped", o.Path, k.Path, o.Path)
			c.Keep = slices.Delete(c.Keep, i, i+1)
			c.add(k)
			return
		}
		if k.Rel == o.Rel {
			c.problem("%s is listed twice: the later one skipped", k.Path)
		} else {
			c.problem("%s overlaps %s: one mount cannot sit inside another, so the later one is skipped", k.Path, o.Path)
		}
		return
	}
	c.Keep = append(c.Keep, k)
}

func (c *Config) parseRoots(v any) []string {
	list, ok := v.([]any)
	if !ok {
		c.problem("roots must be a list of paths in the sandbox")
		return nil
	}
	var roots []string
	for _, e := range list {
		s, ok := e.(string)
		if !ok || !path.IsAbs(s) || path.Clean(s) != s {
			c.problem("roots: %v is not an absolute, clean path in the sandbox, ignored", e)
			continue
		}
		roots = append(roots, s)
	}
	return roots
}

// parseKeep checks the n-th [[keep]] entry.
func (c *Config) parseKeep(n int, e map[string]any) (Keep, bool) {
	p, _ := e["path"].(string)
	where := fmt.Sprintf("keep entry %d", n)
	if p != "" {
		where = fmt.Sprintf("keep %q", p)
	}
	for _, k := range sortedKeys(e) {
		if k != "path" && k != "file" && k != "sync" {
			c.problem("%s: unknown field %q, so the entry is skipped (a newer caboose may know it: caboose update)", where, k)
			return Keep{}, false
		}
	}
	rel, err := keepRel(p)
	if err != nil {
		c.problem("%s: %v; skipped", where, err)
		return Keep{}, false
	}
	k := Keep{Path: p, Rel: rel, Listed: true}
	if v, ok := e["file"]; ok {
		b, ok := v.(bool)
		if !ok {
			c.problem("%s: file must be true or false; skipped", where)
			return Keep{}, false
		}
		k.File = b
	}
	for _, r := range Required {
		if r.Rel == rel {
			if k.File != r.File {
				c.problem("%s: caboose needs it as a %s; taken as one", where, kind(r.File))
			}
			k.File, k.Required = r.File, true
		}
	}
	switch s := e["sync"].(type) {
	case nil:
	case bool:
		if s {
			k.Rules = []Rule{{Path: p, Merge: MergeText, pattern: split(rel)}}
		}
	case map[string]any:
		if r, ok := c.parseRule(where, k, p, s, false); ok {
			k.Rules = []Rule{r}
		}
	case []map[string]any:
		for _, rs := range s {
			if r, ok := c.parseRule(where, k, "", rs, true); ok {
				k.Rules = append(k.Rules, r)
			}
		}
	default:
		c.problem("%s: sync must be true, false, a table, or [[keep.sync]] entries; not synced", where)
	}
	return k, true
}

func kind(file bool) string {
	if file {
		return "file"
	}
	return "directory"
}

// keepRel checks a keep entry's path and returns it relative to the home.
func keepRel(p string) (string, error) {
	rel, ok := strings.CutPrefix(p, "~/")
	if !ok {
		return "", fmt.Errorf("the path must be under the home, written ~/..., not %q", p)
	}
	rel = strings.TrimSuffix(rel, "/")
	if !cleanRel(rel) {
		return "", fmt.Errorf("%q is not a plain path under the home (no ., .., // or the home itself)", p)
	}
	if strings.ContainsAny(rel, `*?[\`) {
		return "", fmt.Errorf("%q: a keep path cannot be a glob; it is one mount", p)
	}
	if !slices.Contains(Parents, parent(rel)) {
		return "", fmt.Errorf("%q is too deep: a keep entry sits directly in ~, ~/.config, ~/.local, ~/.local/share or ~/.cache "+
			"(docker would create the directories above it as root); keep its parent, and sync only what you want of it", p)
	}
	for _, r := range Reserved {
		switch {
		case rel == r:
			return "", fmt.Errorf("~/%s is caboose's own", r)
		case strings.HasPrefix(r, rel+"/"):
			return "", fmt.Errorf("~/%s holds ~/%s, which caboose mounts itself; name the directory inside it you mean", rel, r)
		case strings.HasPrefix(rel, r+"/"):
			return "", fmt.Errorf("~/%s is inside ~/%s, which caboose mounts itself", rel, r)
		}
	}
	return rel, nil
}

// parseRule checks one sync rule of keep k. p is the rule's path when it
// is the entry's own (sync = {...}); sub is set for a [[keep.sync]] entry,
// whose path is its own.
func (c *Config) parseRule(where string, k Keep, p string, e map[string]any, sub bool) (Rule, bool) {
	fail := func(format string, args ...any) (Rule, bool) {
		c.problem("%s: sync %s; that rule is skipped", where, fmt.Sprintf(format, args...))
		return Rule{}, false
	}
	for _, f := range sortedKeys(e) {
		switch f {
		case "merge", "keys", "exclude":
		case "path":
			if !sub {
				return fail("= {...} takes no path: it syncs the entry itself")
			}
		default:
			return fail("has an unknown field %q (a newer caboose may know it: caboose update)", f)
		}
	}
	if sub {
		p, _ = e["path"].(string)
	}
	rel, ok := strings.CutPrefix(p, "~/")
	if !ok || !cleanRel(strings.TrimSuffix(rel, "/")) {
		return fail("path %q is not a path under the home, written ~/...", p)
	}
	rel = strings.TrimSuffix(rel, "/")
	if rel != k.Rel && !strings.HasPrefix(rel, k.Rel+"/") {
		return fail("path %q is not inside %s, spelled out: a rule syncs part of its own entry", p, k.Path)
	}
	r := Rule{Path: p, Merge: MergeText, pattern: split(rel)}
	if err := checkPattern(r.pattern); err != nil {
		return fail("path %q: %v", p, err)
	}
	if v, ok := e["merge"]; ok {
		s, _ := v.(string)
		if !slices.Contains(merges, s) {
			return fail("merge must be one of %s, not %v", strings.Join(merges, ", "), v)
		}
		r.Merge = s
	}
	if v, ok := e["keys"]; ok {
		if r.Merge != MergeJSON {
			return fail("keys only go with merge = \"json\"")
		}
		keys, ok := strings_(v)
		if !ok || len(keys) == 0 {
			return fail("keys must be a list of key names")
		}
		r.Keys = keys
	}
	if v, ok := e["exclude"]; ok {
		ex, ok := strings_(v)
		if !ok {
			return fail("exclude must be a list of paths or names")
		}
		for _, x := range ex {
			if !strings.Contains(x, "/") {
				if x == "" || checkPattern([]string{x}) != nil {
					return fail("exclude %q is not a name or a pattern", x)
				}
				r.excludes, r.names = append(r.excludes, []string{x}), append(r.names, true)
				continue
			}
			xr, ok := strings.CutPrefix(x, "~/")
			if !ok || !cleanRel(strings.TrimSuffix(xr, "/")) {
				return fail("exclude %q: a path is written ~/..., a name without a /", x)
			}
			comps := split(strings.TrimSuffix(xr, "/"))
			if err := checkPattern(comps); err != nil {
				return fail("exclude %q: %v", x, err)
			}
			r.excludes, r.names = append(r.excludes, comps), append(r.names, false)
		}
		r.Exclude = ex
	}
	return r, true
}

// strings_ is v as a list of strings.
func strings_(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// checkPattern refuses a malformed glob component, and a ** that is not a
// component of its own.
func checkPattern(comps []string) error {
	for _, p := range comps {
		if p == "**" || p == RootsComponent {
			continue
		}
		if strings.Contains(p, "**") {
			return errors.New("** has to be a path component of its own")
		}
		if strings.Contains(p, RootsComponent) {
			return errors.New(RootsComponent + " has to be a path component of its own")
		}
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("%q is not a valid pattern", p)
		}
	}
	return nil
}

// Keeps reports the entry holding home-relative path rel, if any.
func (c *Config) Keeps(rel string) (*Keep, bool) {
	for i := range c.Keep {
		if k := &c.Keep[i]; rel == k.Rel || strings.HasPrefix(rel, k.Rel+"/") {
			return k, true
		}
	}
	return nil, false
}

// Match is the rule that syncs the file at home-relative path rel: the
// first rule of its entry that matches it, unless one of that rule's
// excludes does too. False for a file no rule syncs, and for Denied.
func (c *Config) Match(rel string) (*Rule, bool) {
	if slices.Contains(Denied, rel) {
		return nil, false
	}
	k, ok := c.Keeps(rel)
	if !ok {
		return nil, false
	}
	name := split(rel)
	for i := range k.Rules {
		r := &k.Rules[i]
		if !matchPrefix(r.pattern, name, r.roots) {
			continue
		}
		if r.excluded(k.Rel, name) {
			return nil, false
		}
		return r, true
	}
	return nil, false
}

// Reaches reports whether some rule could sync home-relative path rel, or
// something under it, excludes aside: a directory a walk for what syncs has
// to go into, and one that, being a link, is refused rather than followed.
func (c *Config) Reaches(rel string) bool {
	if slices.Contains(Denied, rel) {
		return false
	}
	k, ok := c.Keeps(rel)
	if !ok {
		return false
	}
	name := split(rel)
	for _, r := range k.Rules {
		if reach(r.pattern, name, r.roots) {
			return true
		}
	}
	return false
}

// reach reports whether pat matches name, one of its ancestors, or
// something under it.
func reach(pat, name, roots []string) bool {
	if len(name) == 0 || len(pat) == 0 || pat[0] == "**" {
		return true
	}
	if !matchComp(pat[0], name[0], roots) {
		return false
	}
	return reach(pat[1:], name[1:], roots)
}

// matchComp reports whether pattern component p matches name component n:
// as path.Match, or, for RootsComponent, as the directory of a project
// under one of the roots whose project keys are roots.
func matchComp(p, n string, roots []string) bool {
	if p == RootsComponent {
		for _, k := range roots {
			if n == k || strings.HasPrefix(n, belowKey(k)) {
				return true
			}
		}
		return false
	}
	ok, _ := path.Match(p, n)
	return ok
}

// belowKey is what the key of a project below the root with key k starts
// with: k and the '-' its next '/' becomes, already there for the root /.
func belowKey(k string) string {
	if strings.HasSuffix(k, "-") {
		return k
	}
	return k + "-"
}

// isGlob reports whether pattern component c is more than a literal name.
func isGlob(c string) bool { return c == RootsComponent || strings.ContainsAny(c, `*?[\`) }

// Base is the part of r's path before its first glob, relative to the
// home: the directory (or file) it syncs, when it has no glob.
func (r *Rule) Base() string {
	var lit []string
	for _, c := range r.pattern {
		if isGlob(c) {
			break
		}
		lit = append(lit, c)
	}
	return strings.Join(lit, "/")
}

// Matches reports whether r matches home-relative path rel, excludes
// aside: the file, or a directory holding it.
func (r *Rule) Matches(rel string) bool { return matchPrefix(r.pattern, split(rel), r.roots) }

// excluded reports whether one of r's excludes matches name, a path in the
// entry at keepRel.
func (r *Rule) excluded(keepRel string, name []string) bool {
	below := name[len(split(keepRel)):]
	for i, x := range r.excludes {
		if r.names[i] {
			for _, comp := range below {
				if ok, _ := path.Match(x[0], comp); ok {
					return true
				}
			}
			continue
		}
		if matchPrefix(x, name, r.roots) {
			return true
		}
	}
	return false
}

// matchPrefix reports whether pat matches name or one of its ancestors: a
// pattern naming a directory matches everything in it.
func matchPrefix(pat, name, roots []string) bool {
	for n := len(name); n >= 1; n-- {
		if match(pat, name[:n], roots) {
			return true
		}
	}
	return false
}

// match reports whether pat matches name exactly, component by component:
// ** matches any number of components, anything else one (matchComp).
func match(pat, name, roots []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if match(pat[1:], name[i:], roots) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	if !matchComp(pat[0], name[0], roots) {
		return false
	}
	return match(pat[1:], name[1:], roots)
}

// GitAttributes are the sync repo's merge attributes for c's rules, under
// prefix (the repo's directory for the home): merge=binary for the files
// statesync merges as JSON itself, merge=union for MergeUnion. A rule git
// cannot spell as a pattern (whitespace, quotes) is left out: it then
// merges as text. RootsComponent is spelled out, a pattern for each root.
func (c *Config) GitAttributes(prefix string) string {
	var b strings.Builder
	for _, k := range c.Keep {
		for _, r := range k.Rules {
			attr := ""
			switch r.Merge {
			case MergeJSON:
				attr = "merge=binary"
			case MergeUnion:
				attr = "merge=union"
			default:
				continue
			}
			for _, pat := range expandRoots(r.pattern, r.roots) {
				p := prefix + strings.Join(pat, "/")
				if strings.ContainsAny(p, " \t\"\\#!") {
					continue
				}
				fmt.Fprintf(&b, "%s %s\n%s/** %s\n", p, attr, p, attr)
			}
		}
	}
	return b.String()
}

// expandRoots is pat with each RootsComponent spelled out as plain globs:
// one pattern for each root's key, and one for the keys below it. None for
// a pattern with RootsComponent and no roots, which matches nothing.
func expandRoots(pat, roots []string) [][]string {
	i := slices.Index(pat, RootsComponent)
	if i < 0 {
		return [][]string{pat}
	}
	var out [][]string
	for _, k := range roots {
		for _, c := range []string{k, belowKey(k) + "*"} {
			p := slices.Concat(pat[:i], []string{c}, pat[i+1:])
			out = append(out, expandRoots(p, roots)...)
		}
	}
	return out
}

// cleanRel reports whether rel is a plain relative path: not empty, no
// ., .. or empty components, no leading /.
func cleanRel(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") || path.Clean(rel) != rel || strings.ContainsRune(rel, 0) {
		return false
	}
	for _, c := range strings.Split(rel, "/") {
		if c == "." || c == ".." {
			return false
		}
	}
	return true
}

func split(rel string) []string { return strings.Split(rel, "/") }

// parent is rel's parent directory, "" for one directly in the home.
func parent(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return ""
}

// overlaps reports whether a and b are one path, or one holds the other.
func overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
