package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func wantErr(t *testing.T, body, want string) {
	t.Helper()
	_, err := load(t, body)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%q: err %v, want %q", body, err, want)
	}
}

func TestTrust(t *testing.T) {
	c, err := load(t, "trust = \"hostile\"\n")
	if err != nil || c.Trust != TrustHostile {
		t.Fatalf("%+v %v", c, err)
	}
	if c, err = load(t, ""); err != nil || c.Trust != TrustFallible {
		t.Errorf("default: %q %v", c.Trust, err)
	}
	for _, bad := range []string{"\"paranoid\"", "\"\"", "\"Hostile\"", "true", "1"} {
		wantErr(t, "trust = "+bad+"\n", "trust")
	}
	// Hostile with the outbound proxy explicitly off is refused; unset, or
	// on, or under fallible, is not.
	wantErr(t, "trust = \"hostile\"\n[vm.a]\negress = false\n", "egress = false")
	for _, ok := range []string{
		"trust = \"hostile\"\n[vm.a]\negress = true\n",
		"trust = \"hostile\"\n[vm.a]\ncpus = 2\n",
		"trust = \"fallible\"\n[vm.a]\negress = false\n",
	} {
		if _, err := load(t, ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

func TestRootsWritable(t *testing.T) {
	c, err := load(t, `
[roots]
tools = "~/src/tools"
dev = { host = "~/dev", writable = ["org/a", "org/b"] }
all = { host = "~/all", path = "/opt/all", writable = ["."] }
none = { host = "~/none", writable = [] }
`)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Root{}
	for _, r := range c.Roots {
		by[r.Name] = r
	}
	if len(by["tools"].Writable) != 0 || len(by["none"].Writable) != 0 || by["tools"].Container != "/work/tools" {
		t.Errorf("read-only roots: %+v", by)
	}
	d := by["dev"]
	if !slices.Equal(d.WritableHost(), []string{"/h/dev/org/a", "/h/dev/org/b"}) ||
		!slices.Equal(d.WritableContainer(), []string{"/work/dev/org/a", "/work/dev/org/b"}) || d.WholeWritable() {
		t.Errorf("dev: %+v", d)
	}
	a := by["all"]
	if !a.WholeWritable() || !slices.Equal(a.WritableHost(), []string{"/h/all"}) || !slices.Equal(a.WritableContainer(), []string{"/opt/all"}) {
		t.Errorf("all: %+v", a)
	}
	// Defaults are read-only too.
	c, err = load(t, "")
	if err != nil || len(c.Roots[0].Writable) != 0 {
		t.Errorf("default root: %+v %v", c.Roots, err)
	}
}

func TestRootsWritableRefused(t *testing.T) {
	for name, tc := range map[string]struct{ list, want string }{
		"dotdot":         {`["a/../b"]`, ".."},
		"dotdot alone":   {`[".."]`, ".."},
		"dotdot leading": {`["../x"]`, ".."},
		"absolute":       {`["/etc"]`, "relative"},
		"not clean ./":   {`["./a"]`, "not a clean path"},
		"not clean //":   {`["a//b"]`, "not a clean path"},
		"trailing slash": {`["a/"]`, "not a clean path"},
		"empty":          {`[""]`, "empty"},
		"colon":          {`["a:b"]`, `":"`},
		"comma":          {`["a,b"]`, `","`},
		"backslash":      {`["a\\b"]`, `\`},
		"newline":        {`["a\nb"]`, "control"},
		"nul":            {`["a\u0000b"]`, "control"},
		"bidi":           {`["a\u202eb"]`, "control or invisible"},
		"duplicate":      {`["a", "a"]`, "twice"},
		"nested":         {`["a", "a/b"]`, "inside"},
		"nested reverse": {`["a/b", "a"]`, "inside"},
		"dot and more":   {`[".", "a"]`, "only entry"},
		"not strings":    {`[1]`, "array of paths"},
		"too many":       {`[` + strings.Repeat(`"x", `, 65) + `]`, "at most 64"},
	} {
		wantErr(t, "[roots]\ndev = { host = \"/d\", writable = "+tc.list+" }\n", tc.want)
		_ = name
	}
	wantErr(t, "[roots]\ndev = { host = \"/d\", writable = \"a\" }\n", "array of paths")
	wantErr(t, "[roots]\ndev = { host = \"/d\", mode = \"rw\" }\n", "unknown setting mode")
	wantErr(t, "[roots]\ndev = { writable = [\"a\"] }\n", "root dev must be a path")
	// Similar names that are not nested.
	if _, err := load(t, "[roots]\ndev = { host = \"/d\", writable = [\"a\", \"ab\", \"a-b\"] }\n"); err != nil {
		t.Error(err)
	}
	// FileRoots checks too, for roots that did not come from a file.
	if _, err := FileRoots(map[string]FileRoot{"dev": {Host: "/d", Writable: []string{"../x"}}}, "/h"); err == nil {
		t.Error("FileRoots accepted ..")
	}
}

func TestResolveRootsWritable(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "ok/deep"), filepath.Join(root, "real"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"link": outside, "ok/escape": "../../../outside", "ok/rel": "deep"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	resolve := func(w ...string) error {
		c := &Config{Roots: []Root{{Name: "r", Host: root, Container: "/work/r", Writable: w}}}
		return c.ResolveRoots()
	}
	for _, ok := range [][]string{nil, {"."}, {"ok"}, {"ok/deep"}, {"real", "ok/deep"}} {
		if err := resolve(ok...); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for w, want := range map[string]string{
		"link":      "is a symbolic link",
		"link/x":    "is a symbolic link",
		"ok/escape": "is a symbolic link",
		"ok/rel":    "is a symbolic link",
		"missing":   "does not exist",
		"ok/nope":   "does not exist",
		"file":      "is not a directory",
	} {
		if err := resolve(w); err == nil || !strings.Contains(err.Error(), ", "+want+" (") {
			t.Errorf("%s: %v, want %q", w, err, want)
		}
	}
	// A root that is itself reached through a link is made physical first,
	// and its parts are looked at under the physical path.
	lroot := filepath.Join(base, "lroot")
	if err := os.Symlink(root, lroot); err != nil {
		t.Fatal(err)
	}
	c := &Config{Roots: []Root{{Name: "r", Host: lroot, Container: "/work/r", Writable: []string{"real"}}}}
	if err := c.ResolveRoots(); err != nil || c.Roots[0].Host != root {
		t.Errorf("%v %+v", err, c.Roots)
	}
}

func TestSameWritable(t *testing.T) {
	a := []Root{{Host: "/a", Container: "/work/a", Writable: []string{"x", "y"}}, {Host: "/b", Container: "/work/b"}}
	b := []Root{{Host: "/b", Container: "/work/b"}, {Host: "/a", Container: "/work/a", Writable: []string{"y", "x"}}}
	if !SameWritable(a, b) || !SameRoots(a, b) {
		t.Error("same reported different")
	}
	for _, other := range [][]Root{
		{{Host: "/a", Container: "/work/a", Writable: []string{"x"}}, {Host: "/b", Container: "/work/b"}},
		{{Host: "/a", Container: "/work/a", Writable: []string{"x", "y"}}, {Host: "/b", Container: "/work/b", Writable: []string{"."}}},
		{{Host: "/a", Container: "/work/a"}, {Host: "/b", Container: "/work/b"}},
	} {
		if SameWritable(a, other) {
			t.Errorf("%+v", other)
		}
	}
	// SameRoots leaves writable parts out: a container reports none.
	if !SameRoots(a, []Root{{Host: "/a", Container: "/work/a"}, {Host: "/b", Container: "/work/b"}}) {
		t.Error("SameRoots looked at writable")
	}
}

const goodAlias = `
[alias.caffeinate]
command = ["/usr/bin/caffeinate", "-i"]
description = "keep the Mac awake"
timeout = "90s"
cwd = "~/scratch"
policy = "ask"

[alias.test]
command = ["/usr/local/bin/runner", "--suite", "{suite}", "{mode}"]
[alias.test.params]
suite = ["unit", "all"]
mode = ["fast", "v1.2_x-y"]
`

func TestAliases(t *testing.T) {
	c, err := load(t, goodAlias)
	if err != nil || len(c.Aliases) != 2 {
		t.Fatalf("%+v %v", c, err)
	}
	a, b := c.Aliases[0], c.Aliases[1]
	if a.Name != "caffeinate" || !slices.Equal(a.Command, []string{"/usr/bin/caffeinate", "-i"}) || a.Timeout != 90*time.Second ||
		a.Cwd != "/h/scratch" || a.Policy != AliasAsk || a.Description != "keep the Mac awake" || a.Params != nil {
		t.Errorf("%+v", a)
	}
	if b.Name != "test" || b.Policy != AliasAllow || b.Timeout != 0 || b.Cwd != "" ||
		!slices.Equal(b.Params["suite"], []string{"unit", "all"}) || !slices.Equal(b.Params["mode"], []string{"fast", "v1.2_x-y"}) {
		t.Errorf("%+v", b)
	}
	if c, err = load(t, "[alias.z]\ncommand = [\"/bin/true\"]\ntimeout = \"0\"\ncwd = \"/tmp\"\n"); err != nil || c.Aliases[0].Timeout != 0 || c.Aliases[0].Cwd != "/tmp" {
		t.Errorf("%+v %v", c, err)
	}
}

func TestAliasesRefused(t *testing.T) {
	long := strings.Repeat("x", 201)
	for body, want := range map[string]string{
		"[alias.a]\n":                                          "needs command",
		"[alias.a]\ncommand = []\n":                            "needs command",
		"[alias.a]\ncommand = \"/bin/true\"\n":                 "command in [alias.a] must be an array of strings",
		"[alias.a]\ncommand = [\"true\"]\n":                    "absolute",
		"[alias.a]\ncommand = [\"./true\"]\n":                  "absolute",
		"[alias.a]\ncommand = [\"/bin/../bin/true\"]\n":        "clean",
		"[alias.a]\ncommand = [\"/bin//true\"]\n":              "clean",
		"[alias.a]\ncommand = [\"/\"]\n":                       "clean path to a program",
		"[alias.a]\ncommand = [\"{x}\"]\n":                     "absolute",
		"[alias.a]\ncommand = [\"/bin/true\", \"a\\nb\"]\n":    "control",
		"[alias.a]\ncommand = [\"/bin/true\"]\nshell = true\n": "unknown setting in [alias.a] shell",
		"[alias.a]\ncommand = [\"/bin/true\"]\nargs = []\n":    "unknown setting in [alias.a] args",
		"[alias.A]\ncommand = [\"/bin/true\"]\n":               "not an alias name",
		"[alias.a.b]\ncommand = [\"/bin/true\"]\n":             "unknown setting in [alias.a] b",
		"alias = 1\n": "alias must be tables",
		"[alias.a]\ncommand = [\"/bin/true\"]\npolicy = \"yes\"\n":                     "policy in [alias.a]",
		"[alias.a]\ncommand = [\"/bin/true\"]\npolicy = \"\"\n":                        "policy in [alias.a]",
		"[alias.a]\ncommand = [\"/bin/true\"]\ntimeout = \"soon\"\n":                   "not a duration",
		"[alias.a]\ncommand = [\"/bin/true\"]\ntimeout = \"-1s\"\n":                    "not a duration",
		"[alias.a]\ncommand = [\"/bin/true\"]\ntimeout = 5\n":                          "timeout in [alias.a] must be a string",
		"[alias.a]\ncommand = [\"/bin/true\"]\ncwd = \"rel\"\n":                        "absolute",
		"[alias.a]\ncommand = [\"/bin/true\"]\ncwd = \"~other\"\n":                     "absolute",
		"[alias.a]\ncommand = [\"/bin/true\"]\ncwd = \"/a:b\"\n":                       "absolute",
		"[alias.a]\ncommand = [\"/bin/true\"]\ncwd = \"/a/../b\"\n":                    "clean",
		"[alias.a]\ncommand = [\"/bin/true\"]\ndescription = \"" + long + "\"\n":       "at most 200 bytes",
		"[alias.a]\ncommand = [\"/bin/true\"]\ndescription = \"a\\u202eb\"\n":          "printable",
		"[alias.a]\ncommand = [\"/bin/true\"]\ndescription = \"a\\tb\"\n":              "printable",
		"[alias.a]\ncommand = [\"/bin/true\"]\n[alias.a.params]\nx = []\n":             "1 to 32 values",
		"[alias.a]\ncommand = [\"/bin/true\"]\n[alias.a.params]\nx = \"a\"\n":          "1 to 32 values",
		"[alias.a]\ncommand = [\"/bin/true\"]\n[alias.a.params]\nx = [\"a\", \"a\"]\n": "twice",
		"[alias.a]\ncommand = [\"/bin/true\"]\n[alias.a.params]\nX = [\"a\"]\n":        "not a parameter name",
		"[alias.a]\ncommand = [\"/bin/true\"]\n[alias.a.params]\nx = [1]\n":            "must be an array of strings",
		"[alias.a]\ncommand = [\"/bin/true\"]\nparams = [\"x\"]\n":                     "params in [alias.a] must be a table",
	} {
		wantErr(t, body, want)
	}
	// Parameter values keep a shell and a path out: nothing but letters,
	// digits, . _ and -.
	for _, v := range []string{"a b", "a;b", "$(id)", "a`b`", "a|b", "a&b", "a>b", "a*", "a/b", "../x", "-", "", "é", "a\\nb", "a'b", "a\\\"b", "a{b}", strings.Repeat("x", 65)} {
		body := "[alias.a]\ncommand = [\"/bin/true\"]\n[alias.a.params]\nx = [\"" + strings.ReplaceAll(v, `\`, `\\`) + "\"]\n"
		if v == "-" {
			continue // a plain dash is a legal value
		}
		if _, err := load(t, body); err == nil {
			t.Errorf("param value %q accepted", v)
		}
	}
	vals := make([]string, 33)
	for i := range vals {
		vals[i] = `"v` + strings.Repeat("a", i) + `"`
	}
	wantErr(t, "[alias.a]\ncommand = [\"/bin/true\"]\n[alias.a.params]\nx = ["+strings.Join(vals, ",")+"]\n", "1 to 32 values")
}

func TestEgressDenyAndOpenURLsAllow(t *testing.T) {
	c, err := load(t, `
isolation = "vm.a"
[link]
open_urls_allow = ["https://github.com", "http://localhost:8080", "https://a.b.example:8443"]
[vm.a]
egress_allow = ["git.example"]
egress_deny = ["*.ads.example", "10.0.0.0/8"]

`)
	if err != nil || c.Profile != "vm.a" {
		t.Fatalf("%+v %v", c, err)
	}
	if !slices.Equal(c.EgressDeny, []string{"*.ads.example", "10.0.0.0/8"}) || !slices.Equal(c.EgressAllow, []string{"git.example"}) ||
		!slices.Equal(c.OpenURLsAllow, []string{"https://github.com", "http://localhost:8080", "https://a.b.example:8443"}) {
		t.Errorf("%+v", c)
	}
	wantErr(t, "[container.a]\negress_deny = [\"x\"]\n", "unknown setting in [container.a] egress_deny")
	wantErr(t, "[vm.a]\negress_deny = [1]\n", "array of strings")
	wantErr(t, "[vm.a]\negress_deny = [\"\"]\n", "array of strings")
	for _, bad := range []string{
		"https://github.com/", "https://github.com/x", "https://GitHub.com", "ftp://x.example", "github.com", "https://", "https://u:p@x.example",
		"https://x.example?q", "https://x.example#f", "https://x.example ", "https:x.example", "javascript:alert(1)", "https://x.example\\@y", "file:///etc",
		"https://x.example%2f", "https://x.example:", "*", "https://*.example",
	} {
		wantErr(t, "[link]\nopen_urls_allow = [\""+strings.ReplaceAll(bad, `\`, `\\`)+"\"]\n", "open_urls_allow in [link]")
	}
	wantErr(t, "[link]\nopen_urls_allow = \"https://x.example\"\n", "array of strings")
}

func TestUnknownPolicyKeysRefused(t *testing.T) {
	wantErr(t, "[link]\nopen_urls_allowed = []\n", "unknown setting in [link] open_urls_allowed")
	wantErr(t, "trusted = true\n", "unknown top-level setting trusted")
	wantErr(t, "[aliases.a]\ncommand = []\n", "unknown top-level setting aliases")
}
