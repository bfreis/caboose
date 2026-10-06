package sandboxcfg

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func mustParse(t *testing.T, s string) *Config {
	t.Helper()
	c, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, s)
	}
	return c
}

func TestDefault(t *testing.T) {
	c := mustParse(t, string(Default([]string{"/work/dev", "/opt/oss"})))
	if len(c.Problems) > 0 {
		t.Errorf("problems in the defaults: %q", c.Problems)
	}
	if c.Format != Format || c.Defaults != DefaultsVersion || !slices.Equal(c.Roots, []string{"/work/dev", "/opt/oss"}) {
		t.Errorf("format %d, defaults %d, roots %q", c.Format, c.Defaults, c.Roots)
	}
	var rels []string
	for _, k := range c.Keep {
		rels = append(rels, k.Rel)
		if !k.Listed {
			t.Errorf("%s: not listed", k.Rel)
		}
	}
	want := []string{".claude", ".claude.json", ".config/caboose", ".config/git", ".config/jj", ".config/gh", ".ssh"}
	if !slices.Equal(rels, want) {
		t.Errorf("keeps %q, want %q", rels, want)
	}
	if k, _ := c.Keeps(".claude.json"); !k.File || !k.Required {
		t.Errorf(".claude.json: %+v", k)
	}
	if roots := mustParse(t, string(Default(nil))).Roots; len(roots) != 0 {
		t.Errorf("no roots: %q", roots)
	}

	for rel, merge := range map[string]string{
		".claude/projects/-work-a-b/memory/MEMORY.md": MergeUnion,
		".claude/projects/-work-a-b/memory/one.md":    MergeText,
		".claude/projects/-work/memory/deep/x.md":     MergeText,
		".claude/settings.json":                       MergeJSON,
		".claude/skills/hello/SKILL.md":               MergeText,
		".claude/skills/synced-by-me/SKILL.md":        MergeText,
		".claude/agents/a.md":                         MergeText,
		".claude/commands/deep/c.md":                  MergeText,
		".claude.json":                                MergeJSON,
		".config/caboose/start.d/10-foo":              MergeText,
		".config/caboose/shell.d/aliases.sh":          MergeText,
		".config/caboose/sandbox.toml":                MergeText,
	} {
		r, ok := c.Match(rel)
		if !ok || r.Merge != merge {
			t.Errorf("Match(%q) = %+v, %v; want merge %s", rel, r, ok, merge)
		}
	}
	if r, _ := c.Match(".claude.json"); !slices.Equal(r.Keys, []string{"mcpServers"}) {
		t.Errorf(".claude.json keys %q", r.Keys)
	}
	for _, rel := range []string{
		".claude/.credentials.json",
		".claude/history.jsonl",
		".claude/CLAUDE.md",
		".claude/projects/-work-a/s.jsonl",
		".claude/projects/-home-agent/memory/x.md",
		".claude/skills/synced/acct/docx/SKILL.md",
		".claude/skills/synced",
		".config/git/config",
		".config/gh/hosts.yml",
		".ssh/known_hosts",
		".cargo/env",
		".claudex/x",
	} {
		if r, ok := c.Match(rel); ok {
			t.Errorf("Match(%q) = %+v; want not synced", rel, r)
		}
	}
}

func TestProblems(t *testing.T) {
	for _, tc := range []struct {
		name, toml string
		keeps      []string // beyond the required ones added
		problem    string
	}{
		{"unknown top-level key", `color = "red"`, nil, `unknown setting "color"`},
		{"unknown keep field", "[[keep]]\npath = \"~/.foo\"\nmode = \"x\"", nil, `unknown field "mode"`},
		{"too deep", "[[keep]]\npath = \"~/.config/foo/bar\"", nil, "too deep"},
		{"not under home", "[[keep]]\npath = \"/etc\"", nil, "under the home"},
		{"dotdot", "[[keep]]\npath = \"~/../x\"", nil, "not a plain path"},
		{"glob keep", "[[keep]]\npath = \"~/.f*\"", nil, "cannot be a glob"},
		{"reserved", "[[keep]]\npath = \"~/.local/bin\"", nil, "caboose's own"},
		{"holds reserved", "[[keep]]\npath = \"~/.local\"", nil, "holds ~/.local/bin"},
		{"overlap", "[[keep]]\npath = \"~/.foo\"\n[[keep]]\npath = \"~/.foo\"", []string{".foo"}, "listed twice"},
		{"overlaps required", "[[keep]]\npath = \"~/.config\"", nil, "which caboose needs"},
		{"rule outside", "[[keep]]\npath = \"~/.foo\"\n[[keep.sync]]\npath = \"~/.bar\"", []string{".foo"}, "not inside ~/.foo"},
		{"rule glob before the entry", "[[keep]]\npath = \"~/.foo\"\n[[keep.sync]]\npath = \"~/.f*/x\"", []string{".foo"}, "not inside ~/.foo"},
		{"keys without json", "[[keep]]\npath = \"~/.foo\"\nsync = { keys = [\"a\"] }", []string{".foo"}, "keys only go with"},
		{"bad merge", "[[keep]]\npath = \"~/.foo\"\nsync = { merge = \"ours\" }", []string{".foo"}, "merge must be"},
		{"bad exclude", "[[keep]]\npath = \"~/.foo\"\nsync = { exclude = [\"a/b\"] }", []string{".foo"}, "a path is written ~/"},
		{"bad pattern", "[[keep]]\npath = \"~/.foo\"\n[[keep.sync]]\npath = \"~/.foo/a**\"", []string{".foo"}, "own"},
		{"file type of a required", "[[keep]]\npath = \"~/.claude.json\"", nil, "as a file"},
		{"bad root", `roots = ["dev"]`, nil, "not an absolute, clean path"},
		{"unclean root", `roots = ["/work/../x"]`, nil, "not an absolute, clean path"},
	} {
		c := mustParse(t, tc.toml)
		joined := strings.Join(c.Problems, "\n")
		if !strings.Contains(joined, tc.problem) {
			t.Errorf("%s: problems %q lack %q", tc.name, c.Problems, tc.problem)
		}
		var got []string
		for _, k := range c.Keep {
			if !k.Required {
				got = append(got, k.Rel)
			}
		}
		if !slices.Equal(got, tc.keeps) {
			t.Errorf("%s: keeps %q, want %q", tc.name, got, tc.keeps)
		}
		for _, r := range Required {
			if k, ok := c.Keeps(r.Rel); !ok || k.Rel != r.Rel || k.File != r.File {
				t.Errorf("%s: required %s missing: %+v", tc.name, r.Rel, k)
			}
		}
	}

	for _, bad := range []string{"format = 2", "[[keep]\n", "\xff = 1"} {
		_, err := Parse([]byte(bad))
		if err == nil {
			t.Errorf("Parse(%q) took it", bad)
		}
		if bad == "format = 2" && !errors.Is(err, ErrNewerFormat) {
			t.Errorf("format 2: %v", err)
		}
	}
}

func TestGlobsAndExcludes(t *testing.T) {
	c := mustParse(t, `
[[keep]]
path = "~/.foo"
  [[keep.sync]]
  path = "~/.foo/**/*.toml"
  merge = "text"
  [[keep.sync]]
  path = "~/.foo/data/state.json"
  merge = "json"
  [[keep.sync]]
  path = "~/.foo/data"
  exclude = ["*.tmp", "~/.foo/data/cache", ".git"]
`)
	for _, p := range c.Problems {
		if !strings.Contains(p, "caboose needs it") {
			t.Errorf("problem: %s", p)
		}
	}
	for rel, want := range map[string]string{
		".foo/a.toml":            "~/.foo/**/*.toml",
		".foo/x/y/b.toml":        "~/.foo/**/*.toml",
		".foo/.hidden.toml":      "~/.foo/**/*.toml",
		".foo/data/state.json":   "~/.foo/data/state.json",
		".foo/data/x/y":          "~/.foo/data",
		".foo/data/cache.toml":   "~/.foo/**/*.toml", // first match wins
		".foo/data/cachefile":    "~/.foo/data",
		".foo/data/x/cache/y":    "~/.foo/data", // a path exclude is not a name
		".foo/data/a.tmp":        "",
		".foo/data/cache/y":      "",
		".foo/data/x/.git/HEAD":  "",
		".foo/other":             "",
		".foo/data.toml.bak/x.c": "",
	} {
		r, ok := c.Match(rel)
		got := ""
		if ok {
			got = r.Path
		}
		if got != want {
			t.Errorf("Match(%q) = %q, want %q", rel, got, want)
		}
	}
}

func TestReaches(t *testing.T) {
	c := mustParse(t, string(Default(nil)))
	for rel, want := range map[string]bool{
		".claude":                          true,
		".claude/projects":                 true,
		".claude/projects/-work-a":         true,
		".claude/projects/-work-a/memory":  true,
		".claude/projects/-home-agent":     false,
		".claude/projects/-work-a/x.jsonl": false,
		".claude/todos":                    false,
		".claude/skills/a/b":               true,
		".claude/.credentials.json":        false,
		".config/caboose/start.d":          true,
		".config/git":                      false,
		".cargo":                           false,
	} {
		if got := c.Reaches(rel); got != want {
			t.Errorf("Reaches(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestGitAttributes(t *testing.T) {
	c := mustParse(t, string(Default(nil)))
	got := c.GitAttributes("home/")
	for _, want := range []string{
		"home/.claude/projects/-work*/memory/MEMORY.md merge=union\n",
		"home/.claude/settings.json merge=binary\n",
		"home/.claude/settings.json/** merge=binary\n",
		"home/.claude.json merge=binary\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("attributes lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "skills") {
		t.Errorf("a text rule has an attribute:\n%s", got)
	}
}

func TestAddAndRemoveSync(t *testing.T) {
	base := Default(nil)
	step := func(data []byte, f func([]byte, string) ([]byte, string, error), p string) []byte {
		t.Helper()
		out, msg, err := f(data, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if msg == "" {
			t.Errorf("%s: no message", p)
		}
		return out
	}
	synced := func(data []byte, rel string) bool {
		t.Helper()
		_, ok := mustParse(t, string(data)).Match(rel)
		return ok
	}

	// Part of a kept directory: a [[keep.sync]] in its entry.
	out := step(base, AddSync, "~/.config/git/shared")
	if !synced(out, ".config/git/shared") || synced(out, ".config/git/config") {
		t.Errorf("git/shared:\n%s", out)
	}
	if !strings.Contains(string(out), "path = \"~/.config/git\"\n  [[keep.sync]]\n  path = \"~/.config/git/shared\"\n") {
		t.Errorf("not in the git entry:\n%s", out)
	}
	back := step(out, RemoveSync, "~/.config/git/shared")
	if string(back) != string(base) {
		t.Errorf("remove did not undo add:\n%s", back)
	}

	// A kept directory itself: sync = true after its path.
	out = step(base, AddSync, "/home/agent/.config/jj")
	if !synced(out, ".config/jj/config.toml") || !strings.Contains(string(out), "path = \"~/.config/jj\"\nsync = true\n") {
		t.Errorf("jj:\n%s", out)
	}
	if back := step(out, RemoveSync, "~/.config/jj"); string(back) != string(base) {
		t.Errorf("remove jj:\n%s", back)
	}

	// Nothing keeps it: a new entry.
	out = step(base, AddSync, "~/.cargo")
	c := mustParse(t, string(out))
	if k, ok := c.Keeps(".cargo/env"); !ok || !k.Listed || !synced(out, ".cargo/env") {
		t.Errorf("cargo:\n%s", out)
	}

	// Already synced: unchanged.
	if out, msg, err := AddSync(base, "~/.claude/projects/-work-x/memory/a.md"); err != nil || string(out) != string(base) || !strings.Contains(msg, "already") {
		t.Errorf("already: %v %q", err, msg)
	}
	// Left out by a rule that covers it.
	if _, _, err := AddSync(base, "~/.claude/skills/synced/x"); err == nil || !strings.Contains(err.Error(), "leaves it out") {
		t.Errorf("excluded: %v", err)
	}
	for _, bad := range []string{"~/.config/foo/bar", "~/.claude/.credentials.json", "/etc/x", "rel/x"} {
		if _, _, err := AddSync(base, bad); err == nil {
			t.Errorf("AddSync(%q) took it", bad)
		}
	}

	// Removing a whole rule, a sync = line, and refusing part of a rule.
	out = step(base, RemoveSync, "~/.claude/skills")
	if synced(out, ".claude/skills/x/SKILL.md") || !synced(out, ".claude/agents/a.md") {
		t.Errorf("skills:\n%s", out)
	}
	out = step(base, RemoveSync, "~/.config/caboose")
	if synced(out, ".config/caboose/start.d/x") || !strings.Contains(string(out), "path = \"~/.config/caboose\"\n") {
		t.Errorf("caboose:\n%s", out)
	}
	if _, _, err := RemoveSync(base, "~/.claude/skills/x"); err == nil || !strings.Contains(err.Error(), "as part of ~/.claude/skills") {
		t.Errorf("part of a rule: %v", err)
	}
	if _, _, err := RemoveSync(base, "~/.cargo"); !errors.Is(err, ErrNotSynced) {
		t.Errorf("not synced: %v", err)
	}
}

func TestStamp(t *testing.T) {
	out := string(Stamp([]byte("# c\nformat = 1    # the structure\n\n[[keep]]\npath = \"~/.x\"\n"), 1, 3))
	want := "# c\ndefaults = 3\nformat = 1    # the structure\n\n[[keep]]\npath = \"~/.x\"\n"
	if out != want {
		t.Errorf("Stamp:\n%s\nwant:\n%s", out, want)
	}
	c := mustParse(t, out)
	if c.Defaults != 3 || c.Format != 1 {
		t.Errorf("read back %d %d", c.Format, c.Defaults)
	}
	d := string(Stamp(Default(nil), Format, DefaultsVersion))
	if d != string(Default(nil)) {
		t.Errorf("restamping the defaults changed them:\n%s", d)
	}
}

// The mechanism for a later bump works though nothing is registered now:
// an Addition of a newer defaults version is pending for a file written
// from the first, unless the file lists its path, and a migration brings
// an older format up.
func TestBumpMechanism(t *testing.T) {
	oldAdd, oldMig := Additions, migrations
	t.Cleanup(func() { Additions, migrations = oldAdd, oldMig })
	Additions = []Addition{{Version: DefaultsVersion + 1, Path: "~/.foo", Summary: "foo", Text: "[[keep]]\npath = \"~/.foo\"\n"}}
	c, err := Parse(Default(nil))
	if err != nil {
		t.Fatal(err)
	}
	pending := Pending(c)
	if len(pending) != 1 || pending[0].Path != "~/.foo" {
		t.Fatalf("pending %+v", pending)
	}
	data := Append(Default(nil), pending[0])
	if c, err = Parse(data); err != nil || len(Pending(c)) != 0 {
		t.Errorf("after appending: %v %+v", err, Pending(c))
	}
	if _, err := Migrate([]byte("x"), 0); err == nil {
		t.Error("migrated with no step")
	}
	migrations = map[int]func([]byte) ([]byte, error){0: func(b []byte) ([]byte, error) { return append(b, 'y'), nil }}
	if got, err := Migrate([]byte("x"), 0); err != nil || string(got) != "xy" {
		t.Errorf("migrate: %q %v", got, err)
	}
}
