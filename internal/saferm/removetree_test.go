//go:build unix

package saferm

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func skipRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
}

func mkdirMode(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func modeOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// restoreModes makes dirs (under a t.TempDir) writable again before the
// TempDir's own cleanup runs, so it can remove them.
func restoreModes(t *testing.T, dirs ...string) {
	t.Cleanup(func() {
		for _, d := range dirs {
			_ = os.Chmod(d, 0o700)
		}
	})
}

// outsideDirs makes, under a fresh t.TempDir, a 0555 directory holding a
// file, a 0000 directory and a 0444 file, and returns their paths.
func outsideDirs(t *testing.T) (ro, zero, file string) {
	t.Helper()
	out := t.TempDir()
	ro, zero, file = filepath.Join(out, "ro"), filepath.Join(out, "zero"), filepath.Join(out, "file")
	mk(t, filepath.Join(ro, "keep"))
	mk(t, file)
	if err := os.Chmod(file, 0o444); err != nil {
		t.Fatal(err)
	}
	mkdirMode(t, ro, 0o555)
	mkdirMode(t, zero, 0o000)
	restoreModes(t, ro, zero)
	return
}

func TestRemoveTreeReadOnlyDirs(t *testing.T) {
	skipRoot(t)
	root := t.TempDir()
	top := filepath.Join(root, "top")
	for _, d := range []string{"a/b", "c", "d/e"} {
		mk(t, filepath.Join(top, d, "f"))
	}
	mkdirMode(t, filepath.Join(top, "a", "b"), 0o000)
	mkdirMode(t, filepath.Join(top, "a"), 0o555)
	mkdirMode(t, filepath.Join(top, "c"), 0o300)
	mkdirMode(t, filepath.Join(top, "d", "e"), 0o555)
	mkdirMode(t, filepath.Join(top, "d"), 0o000)
	mkdirMode(t, top, 0o555)
	if err := RemoveTree(root, top); err != nil {
		t.Fatal(err)
	}
	if exists(top) || !exists(root) {
		t.Fatal("tree not removed, or root removed")
	}
	// A file, and a missing path.
	mk(t, filepath.Join(root, "file"))
	if err := RemoveTree(root, filepath.Join(root, "file")); err != nil || exists(filepath.Join(root, "file")) {
		t.Fatal("file not removed", err)
	}
	if err := RemoveTree(root, filepath.Join(root, "missing")); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveTreeSymlinksOutside(t *testing.T) {
	skipRoot(t)
	ro, zero, file := outsideDirs(t)
	root := t.TempDir()
	top := filepath.Join(root, "top")
	if err := os.MkdirAll(filepath.Join(top, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"lro": ro, "lzero": zero, "lfile": file} {
		if err := os.Symlink(target, filepath.Join(top, "sub", name)); err != nil {
			t.Fatal(err)
		}
	}
	mkdirMode(t, filepath.Join(top, "sub"), 0o555)
	mkdirMode(t, top, 0o000)
	if err := RemoveTree(root, top); err != nil {
		t.Fatal(err)
	}
	if exists(top) {
		t.Fatal("tree not removed")
	}
	if m := modeOf(t, ro); m != 0o555 {
		t.Errorf("outside 0555 dir now %o", m)
	}
	if m := modeOf(t, zero); m != 0 {
		t.Errorf("outside 0000 dir now %o", m)
	}
	if m := modeOf(t, file); m != 0o444 {
		t.Errorf("outside file now %o", m)
	}
	if !exists(filepath.Join(ro, "keep")) {
		t.Error("outside content removed")
	}
}

// TestRemoveTreeSwap swaps a directory for a symlink to outside between
// its fstatat and its open: the target outside is not changed.
func TestRemoveTreeSwap(t *testing.T) {
	skipRoot(t)
	for _, mode := range []os.FileMode{0o555, 0o000} {
		ro, zero, _ := outsideDirs(t)
		target := ro
		if mode == 0 {
			target = zero
		}
		root := t.TempDir()
		top := filepath.Join(root, "top")
		mk(t, filepath.Join(top, "d", "f"))
		mkdirMode(t, filepath.Join(top, "d"), mode)
		moved := filepath.Join(root, "moved")
		swapped := false
		testHookAfterStat = func(dir *os.File, name string) {
			if name != "d" || swapped {
				return
			}
			swapped = true
			// A directory is renamed only when writable (its ".." changes).
			_ = os.Chmod(filepath.Join(top, "d"), 0o700)
			if err := os.Rename(filepath.Join(top, "d"), moved); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(target, filepath.Join(top, "d")); err != nil {
				t.Error(err)
			}
			_ = os.Chmod(moved, mode)
		}
		err := RemoveTree(root, top)
		testHookAfterStat = nil
		if !swapped {
			t.Fatal("hook not called")
		}
		if err != nil {
			t.Fatalf("mode %o: %v", mode, err)
		}
		if m := modeOf(t, target); m != mode {
			t.Errorf("mode %o: outside target now %o", mode, m)
		}
		if exists(top) || !exists(target) {
			t.Errorf("mode %o: link not removed alone", mode)
		}
		if err := RemoveTree(root, moved); err != nil || exists(moved) {
			t.Fatal("moved dir not removed", err)
		}
	}
}

func TestRemoveTreeRefusals(t *testing.T) {
	skipRoot(t)
	base, root, outside := layout(t)
	ro := filepath.Join(outside, "ro")
	mk(t, filepath.Join(ro, "keep"))
	mkdirMode(t, ro, 0o555)
	restoreModes(t, ro)
	mk(t, filepath.Join(root, "in", "f"))
	mkdirMode(t, filepath.Join(root, "in"), 0o555)
	restoreModes(t, filepath.Join(root, "in"))
	if err := os.Symlink(outside, filepath.Join(root, "up")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"root itself":     root,
		"root with dot":   filepath.Join(root, "."),
		"parent":          base,
		"outside":         ro,
		"dotdot escape":   filepath.Join(root, "..", "outside", "ro"),
		"through symlink": filepath.Join(root, "up", "ro"),
		"relative":        filepath.Join("outside", "ro"),
	}
	for name, p := range cases {
		if err := RemoveTree(root, p); !errors.Is(err, ErrOutside) {
			t.Errorf("%s: want ErrOutside, got %v", name, err)
		}
	}
	if err := RemoveTree(root, ""); err == nil {
		t.Error("empty path accepted")
	}
	if err := RemoveTree("", filepath.Join(root, "in")); err == nil {
		t.Error("empty root accepted")
	}
	if !exists(filepath.Join(ro, "keep")) || modeOf(t, ro) != 0o555 || modeOf(t, outside) != 0o700 {
		t.Error("outside changed")
	}
	if !exists(filepath.Join(root, "in", "f")) || modeOf(t, filepath.Join(root, "in")) != 0o555 {
		t.Error("inside changed by a refusal")
	}
}

// TestRemoveTreeSwapAfterOpen swaps the directory for a symlink to outside
// after it was opened and before its mode is changed: the change goes
// through the descriptor, to the directory opened, never to the target.
func TestRemoveTreeSwapAfterOpen(t *testing.T) {
	skipRoot(t)
	ro, _, _ := outsideDirs(t)
	root := t.TempDir()
	top := filepath.Join(root, "top")
	mk(t, filepath.Join(top, "d", "f"))
	mkdirMode(t, filepath.Join(top, "d"), 0o555)
	moved := filepath.Join(root, "moved")
	swapped := false
	testHookAfterOpen = func(dir *os.File, name string) {
		if name != "d" || swapped {
			return
		}
		swapped = true
		_ = os.Chmod(filepath.Join(top, "d"), 0o700)
		if err := os.Rename(filepath.Join(top, "d"), moved); err != nil {
			t.Error(err)
		}
		_ = os.Chmod(moved, 0o555)
		if err := os.Symlink(ro, filepath.Join(top, "d")); err != nil {
			t.Error(err)
		}
	}
	err := RemoveTree(root, top)
	testHookAfterOpen = nil
	if !swapped || err != nil {
		t.Fatalf("swapped %v: %v", swapped, err)
	}
	if m := modeOf(t, ro); m != 0o555 {
		t.Errorf("outside target now %o", m)
	}
	if exists(top) || !exists(filepath.Join(ro, "keep")) {
		t.Error("link not removed alone")
	}
	if err := RemoveTree(root, moved); err != nil || exists(moved) {
		t.Fatal("moved dir not removed", err)
	}
}

// TestRemoveTreeSwapOtherDir swaps another real directory in between the
// fstatat and the open: its device and inode differ, so it is not changed.
func TestRemoveTreeSwapOtherDir(t *testing.T) {
	skipRoot(t)
	root := t.TempDir()
	top := filepath.Join(root, "top")
	mk(t, filepath.Join(top, "d", "f"))
	other := filepath.Join(t.TempDir(), "other")
	mk(t, filepath.Join(other, "keep"))
	mkdirMode(t, other, 0o555)
	moved := filepath.Join(root, "moved")
	swapped := false
	testHookAfterStat = func(dir *os.File, name string) {
		if name != "d" || swapped {
			return
		}
		swapped = true
		if err := os.Rename(filepath.Join(top, "d"), moved); err != nil {
			t.Error(err)
		}
		_ = os.Chmod(other, 0o700)
		if err := os.Rename(other, filepath.Join(top, "d")); err != nil {
			t.Error(err)
		}
		_ = os.Chmod(filepath.Join(top, "d"), 0o555)
	}
	restoreModes(t, filepath.Join(top, "d"))
	_ = RemoveTree(root, top) // the swapped-in directory is read-only: removal fails
	testHookAfterStat = nil
	if !swapped {
		t.Fatal("hook not called")
	}
	if m := modeOf(t, filepath.Join(top, "d")); m != 0o555 {
		t.Errorf("swapped-in directory now %o", m)
	}
	if !exists(filepath.Join(top, "d", "keep")) {
		t.Error("swapped-in directory emptied")
	}
}

func TestRemoveTreePathIsSymlink(t *testing.T) {
	skipRoot(t)
	ro, _, _ := outsideDirs(t)
	root := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(ro, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(root, link); err != nil {
		t.Fatal(err)
	}
	if exists(link) || !exists(filepath.Join(ro, "keep")) || modeOf(t, ro) != 0o555 {
		t.Fatal("link not removed alone")
	}
}

func TestRemoveTreeDepth(t *testing.T) {
	old := maxTreeDepth
	maxTreeDepth = 3
	t.Cleanup(func() { maxTreeDepth = old })
	root := t.TempDir()
	mk(t, filepath.Join(root, "a", "b", "c", "d", "f"))
	err := RemoveTree(root, filepath.Join(root, "a"))
	if err == nil || !strings.Contains(err.Error(), "deeper") {
		t.Fatalf("want depth error, got %v", err)
	}
	if !exists(filepath.Join(root, "a", "b", "c", "d", "f")) {
		t.Fatal("removed past the depth limit")
	}
	mk(t, filepath.Join(root, "x", "y", "f"))
	if err := RemoveTree(root, filepath.Join(root, "x")); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveTreeNoProc(t *testing.T) {
	skipRoot(t)
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	setProc(t, filepath.Join(t.TempDir(), "noproc"))
	root := t.TempDir()
	mk(t, filepath.Join(root, "z", "f"))
	mkdirMode(t, filepath.Join(root, "z"), 0)
	err := RemoveTree(root, filepath.Join(root, "z"))
	if err != nil {
		// No fchmodat2 (before Linux 6.6): a clear error, the tree left.
		if !strings.Contains(err.Error(), "without /proc") {
			t.Fatalf("unclear error: %v", err)
		}
		restoreModes(t, filepath.Join(root, "z"))
		return
	}
	if exists(filepath.Join(root, "z")) {
		t.Fatal("not removed")
	}
}
