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

// A root inside CABOOSE_HOME, under a second name for it, is refused when
// the roots are resolved: it would hand the sandbox the daemon's token
// and every environment's configuration.
func TestResolveRootsRefusesInsideCabooseHome(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	u := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(u, ".caboose", "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(u, ".caboose"), filepath.Join(dir, "second")); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{filepath.Join(u, ".caboose"), filepath.Join(dir, "second")} {
		c := &Config{CabooseHome: ch, Home: u,
			Roots: []Root{{Name: "d", Host: filepath.Join(u, ".caboose", "daemon"), Container: "/work/d"}}}
		if err := c.ResolveRoots(); err == nil || !strings.Contains(err.Error(), "is inside CABOOSE_HOME") {
			t.Errorf("CABOOSE_HOME %s: %v", ch, err)
		}
	}
}

// A root written as a real directory in another root's writable part,
// which the sandbox then swaps for a symlink to ~/.ssh: the next launch
// refuses it rather than mount where the link leads.
func TestResolveRootsRefusesALinkInAWritablePart(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(base, "home")
	ssh := filepath.Join(home, ".ssh")
	x := filepath.Join(home, "dev", "proj", "x")
	for _, d := range []string{ssh, x, filepath.Join(base, "ch")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	roots := func() []Root {
		return []Root{
			{Name: "dev", Host: filepath.Join(home, "dev"), Container: "/work/dev", Writable: []string{"proj"}},
			{Name: "x", Host: x, Container: "/work/x"},
		}
	}
	c := &Config{Home: home, CabooseHome: filepath.Join(base, "ch"), Roots: roots()}
	if err := c.ResolveRoots(); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("a root in another, before the swap: %v", err)
	}
	if err := os.Remove(x); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ssh, x); err != nil {
		t.Fatal(err)
	}
	c = &Config{Home: home, CabooseHome: filepath.Join(base, "ch"), Roots: roots()}
	err := c.ResolveRoots()
	if err == nil || !strings.Contains(err.Error(), "symbolic link "+x) || !strings.Contains(err.Error(), "writable part") {
		t.Fatalf("after the swap: %v (roots %+v)", err, c.Roots)
	}
	// A symlink outside every writable part is followed, as before, and
	// kept in Via for the check against every sandbox's.
	elsewhere := filepath.Join(base, "projects")
	if err := os.Symlink(filepath.Join(home, "dev"), elsewhere); err != nil {
		t.Fatal(err)
	}
	c = &Config{Home: home, CabooseHome: filepath.Join(base, "ch"), Roots: []Root{{Name: "p", Host: elsewhere, Container: "/work/p"}}}
	if err := c.ResolveRoots(); err != nil || c.Roots[0].Host != filepath.Join(home, "dev") || len(c.Roots[0].Via) != 1 || c.Roots[0].Via[0] != elsewhere {
		t.Fatalf("%v %+v", err, c.Roots)
	}
	if why := LinkInPaths(c.Roots[0], []string{base}); !strings.Contains(why, "symbolic link "+elsewhere) {
		t.Errorf("LinkInPaths: %q", why)
	}
	if why := LinkInPaths(c.Roots[0], []string{filepath.Join(home, "dev")}); why != "" {
		t.Errorf("LinkInPaths, a writable path not holding the link: %q", why)
	}
}

// physicalVia resolves as EvalSymlinks does, relative and chained links
// and .. included, and lists every link met.
func TestPhysicalVia(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(base, "a", "b", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a/b", filepath.Join(base, "l1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "l1"), filepath.Join(base, "l2")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(base, "l2", "c"), filepath.Join(base, "l1", "c", "..", "c"), filepath.Join(base, "a", "b", "c")} {
		want, _ := filepath.EvalSymlinks(p)
		got, via, err := physicalVia(p)
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", p, got, err, want)
		}
		t.Logf("%s via %v", p, via)
	}
	if _, via, _ := physicalVia(filepath.Join(base, "l2", "c")); len(via) != 2 {
		t.Errorf("via %v", via)
	}
	if err := os.Symlink("loop", filepath.Join(base, "loop")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := physicalVia(filepath.Join(base, "loop")); err == nil {
		t.Error("a loop resolved")
	}
}
