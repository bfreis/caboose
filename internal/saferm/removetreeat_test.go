//go:build unix

package saferm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// underTemp fails the test unless p is under os.TempDir(), before any
// removal is tried with p as its root.
func underTemp(t *testing.T, p string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(tmp, rp); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		t.Fatalf("%s is not under %s", rp, tmp)
	}
}

func openDir(t *testing.T, p string) *os.File {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestRemoveTreeAt(t *testing.T) {
	skipRoot(t)
	base := t.TempDir()
	underTemp(t, base)
	ro, zero, file := outsideDirs(t)
	top := filepath.Join(base, "top")
	tree := filepath.Join(top, "tree")
	mk(t, filepath.Join(tree, "a", "b", "f"))
	for i := range 2100 { // more than one listing batch
		mk(t, filepath.Join(tree, "many", fmt.Sprint(i)))
	}
	mkdirMode(t, filepath.Join(tree, "zero"), 0o700)
	mk(t, filepath.Join(tree, "zero", "x"))
	for _, l := range []struct{ name, to string }{{"lro", ro}, {"lzero", zero}, {"lfile", file}} {
		if err := os.Symlink(l.to, filepath.Join(tree, l.name)); err != nil {
			t.Fatal(err)
		}
	}
	mkdirMode(t, filepath.Join(tree, "a", "b"), 0o555)
	mkdirMode(t, filepath.Join(tree, "zero"), 0o000)
	restoreModes(t, filepath.Join(tree, "a", "b"), filepath.Join(tree, "zero"))
	mk(t, filepath.Join(top, "sibling"))

	if err := RemoveTreeAt(openDir(t, top), "tree"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tree still there: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(top, "sibling")); err != nil {
		t.Error("a sibling was removed")
	}
	if _, err := os.Lstat(filepath.Join(ro, "keep")); err != nil {
		t.Error("a symlink's target directory lost its contents")
	}
	if m := modeOf(t, ro); m != 0o555 {
		t.Errorf("outside 0555 dir is now %o", m)
	}
	if m := modeOf(t, zero); m != 0o000 {
		t.Errorf("outside 0000 dir is now %o", m)
	}
	if m := modeOf(t, file); m != 0o444 {
		t.Errorf("outside file is now %o", m)
	}
	// Gone already: not an error.
	if err := RemoveTreeAt(openDir(t, top), "tree"); err != nil {
		t.Errorf("missing name: %v", err)
	}
}

// The root is the directory held, not its path: a path above it swapped for
// a symlink to another directory does not move the removal there.
func TestRemoveTreeAtHeldRoot(t *testing.T) {
	base := t.TempDir()
	underTemp(t, base)
	held := filepath.Join(base, "held")
	mk(t, filepath.Join(held, "out", "f"))
	victim := filepath.Join(base, "victim")
	mk(t, filepath.Join(victim, "out", "keep"))
	dir := openDir(t, held)
	if err := os.Rename(held, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, held); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTreeAt(dir, "out"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(victim, "out", "keep")); err != nil {
		t.Errorf("the symlink's target was removed from: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "moved", "out")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the held directory's entry is still there: %v", err)
	}
}

func TestRemoveTreeAtRefuses(t *testing.T) {
	base := t.TempDir()
	underTemp(t, base)
	in := filepath.Join(base, "in")
	mk(t, filepath.Join(in, "sub", "f"))
	mk(t, filepath.Join(base, "keep"))
	dir := openDir(t, in)
	for _, name := range []string{"", ".", "..", "sub/f", "../keep", "/", "a\x00b"} {
		if err := RemoveTreeAt(dir, name); !errors.Is(err, ErrOutside) {
			t.Errorf("%q: %v, want ErrOutside", name, err)
		}
	}
	if err := RemoveTreeAt(nil, "sub"); !errors.Is(err, ErrOutside) {
		t.Errorf("nil dir: %v", err)
	}
	for _, p := range []string{filepath.Join(in, "sub", "f"), filepath.Join(base, "keep")} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed on a refusal", p)
		}
	}
	// A symlink at the name is unlinked, its target left.
	if err := os.Symlink(filepath.Join(base, "keep"), filepath.Join(in, "link")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTreeAt(dir, "link"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(base, "keep")); err != nil {
		t.Error("a symlink's target was removed")
	}
}

// A directory swapped for another between its stat and its open is not
// emptied: the removal stops with an error and the newcomer is intact.
func TestRemoveTreeAtSwapped(t *testing.T) {
	base := t.TempDir()
	underTemp(t, base)
	top := filepath.Join(base, "top")
	mk(t, filepath.Join(top, "tree", "d", "f"))
	other := filepath.Join(base, "other")
	mk(t, filepath.Join(other, "keep"))
	t.Cleanup(func() { testHookRemoveAfterStat = nil })
	swapped := false
	testHookRemoveAfterStat = func(dir *os.File, name string) {
		if name != "d" || swapped {
			return
		}
		swapped = true
		d := filepath.Join(top, "tree", "d")
		if err := os.Rename(d, filepath.Join(base, "d.away")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(other, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveTreeAt(openDir(t, top), "tree"); err == nil {
		t.Fatal("removed a tree whose directory was swapped mid-removal")
	}
	if _, err := os.Lstat(filepath.Join(top, "tree", "d", "keep")); err != nil {
		t.Errorf("the swapped-in directory was emptied: %v", err)
	}
}
