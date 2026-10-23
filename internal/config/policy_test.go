package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func wantErr(t *testing.T, body, want string) {
	t.Helper()
	_, err := load(t, body)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%q: err %v, want %q", body, err, want)
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
