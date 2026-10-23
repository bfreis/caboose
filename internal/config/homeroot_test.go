package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHoldsDir(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	for _, d := range []string{"u/.caboose", "u/dev", "ub", "real"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// alias is a second name for real; CABOOSE_HOME under it is missing.
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	u := filepath.Join(dir, "u")
	for _, tc := range []struct {
		root, home string
		want       bool
	}{
		{u, filepath.Join(u, ".caboose"), true},
		{filepath.Join(u, ".caboose"), filepath.Join(u, ".caboose"), true},
		{dir, filepath.Join(u, ".caboose"), true},
		{"/", filepath.Join(u, ".caboose"), true},
		{filepath.Join(u, "dev"), filepath.Join(u, ".caboose"), false},
		{filepath.Join(dir, "ub"), filepath.Join(u, ".caboose"), false},
		{u, filepath.Join(u, "missing", "caboose"), true},
		// Through a symlink above a CABOOSE_HOME not made yet: by the
		// nearest existing ancestor's physical path and identity.
		{filepath.Join(dir, "real"), filepath.Join(dir, "alias", "caboose"), true},
		{filepath.Join(dir, "alias"), filepath.Join(dir, "real", "caboose"), true},
		{filepath.Join(dir, "real"), filepath.Join(u, ".caboose"), false},
		{u, "", false},
		{"relative", filepath.Join(u, ".caboose"), false},
	} {
		if got := HoldsDir(tc.root, tc.home); got != tc.want {
			t.Errorf("HoldsDir(%s, %s) = %v", tc.root, tc.home, got)
		}
	}
}

// A root that holds CABOOSE_HOME is refused when the roots are resolved,
// naming a narrower root: a preferred subdirectory, else the first, never
// a hidden one or one that holds CABOOSE_HOME too.
func TestResolveRootsRefusesCabooseHome(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	u := filepath.Join(dir, "home")
	for _, d := range []string{".caboose", ".config", "Desktop", "dev"} {
		if err := os.MkdirAll(filepath.Join(u, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := &Config{CabooseHome: filepath.Join(u, ".caboose"), Home: u,
		Roots: []Root{{Name: "all", Host: u, Container: "/work/all"}}}
	err := c.ResolveRoots()
	if err == nil || !strings.Contains(err.Error(), "holds CABOOSE_HOME") || !strings.Contains(err.Error(), "use ~/dev instead of ~") {
		t.Fatalf("%v", err)
	}
	if err := os.Remove(filepath.Join(u, "dev")); err != nil {
		t.Fatal(err)
	}
	if why := CabooseHomeRootProblem(u, c.CabooseHome, u); !strings.Contains(why, "use ~/Desktop instead of ~") {
		t.Errorf("%q", why)
	}
	if why := CabooseHomeRootProblem(dir, c.CabooseHome, u); !strings.Contains(why, "name a directory under "+dir) {
		t.Errorf("only home under it, which holds CABOOSE_HOME: %q", why)
	}
	// A root that is CABOOSE_HOME is never told to use a directory in it.
	if err := os.MkdirAll(filepath.Join(c.CabooseHome, "envs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if why := CabooseHomeRootProblem(c.CabooseHome, c.CabooseHome, u); !strings.Contains(why, "is CABOOSE_HOME") || strings.Contains(why, "envs") {
		t.Errorf("the root is CABOOSE_HOME: %q", why)
	}
	// CABOOSE_HOME not made yet still counts.
	c = &Config{CabooseHome: filepath.Join(u, "later", "caboose"), Home: u,
		Roots: []Root{{Name: "all", Host: u, Container: "/work/all"}}}
	if err := c.ResolveRoots(); err == nil || !strings.Contains(err.Error(), "holds CABOOSE_HOME") {
		t.Fatalf("missing CABOOSE_HOME: %v", err)
	}
	// A root beside it is fine.
	c = &Config{CabooseHome: filepath.Join(u, ".caboose"), Home: u,
		Roots: []Root{{Name: "desk", Host: filepath.Join(u, "Desktop"), Container: "/work/desk"}}}
	if err := c.ResolveRoots(); err != nil {
		t.Fatal(err)
	}
}
