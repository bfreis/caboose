//go:build darwin

package saferm

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func flagsOf(t *testing.T, p string) uint32 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return st.Flags
}

func chflags(t *testing.T, p string, flags int) {
	t.Helper()
	if err := unix.Chflags(p, flags); err != nil {
		t.Fatal(err)
	}
	// Cleared again before the TempDir's cleanup, should the test fail.
	t.Cleanup(func() { _ = unix.Chflags(p, 0) })
}

func TestRemoveTreeFlags(t *testing.T) {
	skipRoot(t)
	out := t.TempDir()
	target := filepath.Join(out, "target")
	mk(t, target)
	chflags(t, target, unix.UF_IMMUTABLE)

	root := t.TempDir()
	top := filepath.Join(root, "top")
	mk(t, filepath.Join(top, "imm", "f"))
	mk(t, filepath.Join(top, "app", "g"))
	mk(t, filepath.Join(top, "zero", "h"))
	if err := os.Symlink(target, filepath.Join(top, "link")); err != nil {
		t.Fatal(err)
	}
	chflags(t, filepath.Join(top, "imm", "f"), unix.UF_IMMUTABLE)
	chflags(t, filepath.Join(top, "app"), unix.UF_APPEND)
	mkdirMode(t, filepath.Join(top, "zero"), 0)
	restoreModes(t, filepath.Join(top, "zero"))

	if err := RemoveTree(root, top); err != nil {
		t.Fatal(err)
	}
	if exists(top) {
		t.Fatal("tree not removed")
	}
	if flagsOf(t, target)&unix.UF_IMMUTABLE == 0 {
		t.Error("outside target's flags cleared")
	}
	if !exists(target) {
		t.Error("outside target removed")
	}
}
