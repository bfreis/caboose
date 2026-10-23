package checkpoint

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// A setgid or sticky directory, like a setuid file, loses the bit in the copy.
func TestCopyDropsDirBits(t *testing.T) {
	skipRoot(t)
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "src", "sgid", "f"), "f", 0o644)
	mustWrite(t, filepath.Join(top, "src", "sticky", "f"), "f", 0o644)
	if err := unix.Chmod(filepath.Join(top, "src", "sgid"), 0o2755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chmod(filepath.Join(top, "src", "sticky"), 0o1777); err != nil {
		t.Fatal(err)
	}
	if m := lstat(t, filepath.Join(top, "src", "sgid")).Mode & 0o7777; m != 0o2755 {
		t.Skipf("cannot set setgid here (mode %o)", m)
	}
	root, dst, _ := dstFor(t)
	if _, err := copyT(t, context.Background(), rootSource(top, "src"), dst, Options{Root: root, NoWhole: true}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]uint32{"sgid": 0o755, "sticky": 0o777} {
		if m := uint32(lstat(t, filepath.Join(dst, p)).Mode) & 0o7777; m != want {
			t.Errorf("%s is %o, want %o", p, m, want)
		}
	}
}

// A tree deeper than the walker's bound is copied down to it; the
// directory past it is skipped and counted.
func TestCopyDepthBound(t *testing.T) {
	top := tempTree(t)
	rel := "src"
	for range maxDepth + 2 {
		rel += "/d"
	}
	mustWrite(t, filepath.Join(top, filepath.FromSlash(rel), "f"), "deep", 0o644)
	root, dst, _ := dstFor(t)
	r, err := copyT(t, context.Background(), rootSource(top, "src"), dst, Options{Root: root, NoWhole: true})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSuffix(strings.Repeat("d/", maxDepth+1), "/")
	if r.Skipped != 1 || len(r.SkippedExamples) != 1 || r.SkippedExamples[0] != want {
		t.Fatalf("skipped %d %v", r.Skipped, r.SkippedExamples)
	}
	if r.Entries != maxDepth {
		t.Errorf("entries %d, want %d", r.Entries, maxDepth)
	}
	last := filepath.Join(dst, filepath.FromSlash(strings.TrimSuffix(strings.Repeat("d/", maxDepth), "/")))
	if fi, err := os.Lstat(last); err != nil || !fi.IsDir() {
		t.Fatalf("level %d: %v", maxDepth, err)
	}
	if _, err := os.Lstat(filepath.Join(last, "d")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("level %d copied: %v", maxDepth+1, err)
	}
}

// When the source's name in its parent no longer holds what was opened,
// no whole-tree clone is made by that name: the walker copies what was
// opened.
func TestSourceParentRecheck(t *testing.T) {
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "src", "orig"), "opened", 0o644)
	calls := fakeWhole(t, true, func(*os.File, string, *os.File, string) error { return nil })
	testHookSourceOpened = func() {
		if err := os.Rename(filepath.Join(top, "src"), filepath.Join(top, "was-src")); err != nil {
			t.Error(err)
		}
		mustWrite(t, filepath.Join(top, "src", "swapped"), "other", 0o644)
	}
	t.Cleanup(func() { testHookSourceOpened = nil })
	root, dst, _ := dstFor(t)
	r, err := copyT(t, context.Background(), rootSource(top, "src"), dst, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 0 || r.Method == MethodClonefile {
		t.Fatalf("cloned by name after a swap (%d calls, %s)", *calls, r.Method)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "orig")); err != nil || string(b) != "opened" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "swapped")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the swapped-in directory was copied")
	}
}

// A login file under a copied parent that denies its owner everything but
// reading (0400) or everything (0000) is still dropped, and the modes set
// back.
func TestDropLoginLockedParents(t *testing.T) {
	skipRoot(t)
	for _, mode := range []os.FileMode{0o400, 0o000, 0o500} {
		tmp := tempTree(t)
		tree := filepath.Join(tmp, TreeName)
		mustWrite(t, filepath.Join(tree, "a", ".credentials.json"), "x", 0o600)
		mustWrite(t, filepath.Join(tree, "a", "keep"), "k", 0o600)
		if err := os.Chmod(filepath.Join(tree, "a"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(tree, mode); err != nil {
			t.Fatal(err)
		}
		src := Source{Path: "~/x", Kind: SourceHome, Top: "/nowhere", Rel: "home/x"}
		got, err := dropLogin(tmp, src, []string{"x/a/.credentials.json"})
		if err != nil {
			t.Fatalf("%o: %v", mode, err)
		}
		if len(got) != 1 || got[0] != "~/x/a/.credentials.json" {
			t.Fatalf("%o: dropped %v", mode, got)
		}
		if m := uint32(lstat(t, tree).Mode) & 0o7777; m != uint32(mode) {
			t.Errorf("%o: tree back as %o", mode, m)
		}
		os.Chmod(tree, 0o700)
		if m := uint32(lstat(t, filepath.Join(tree, "a")).Mode) & 0o7777; m != uint32(mode) {
			t.Errorf("%o: a back as %o", mode, m)
		}
		os.Chmod(filepath.Join(tree, "a"), 0o700)
		if _, err := os.Lstat(filepath.Join(tree, "a", ".credentials.json")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%o: still there", mode)
		}
		if _, err := os.Lstat(filepath.Join(tree, "a", "keep")); err != nil {
			t.Errorf("%o: keep: %v", mode, err)
		}
	}
}

// meta.json failing for want of space is ErrDiskFull, and the take
// leaves nothing.
func TestWriteFileErrDiskFull(t *testing.T) {
	for _, e := range []error{unix.ENOSPC, unix.EDQUOT} {
		if err := writeFileErr(&os.PathError{Op: "write", Path: "meta.json", Err: e}); !errors.Is(err, ErrDiskFull) || !errors.Is(err, e) {
			t.Errorf("%v: %v", e, err)
		}
	}
	if err := writeFileErr(unix.EIO); errors.Is(err, ErrDiskFull) {
		t.Errorf("EIO: %v", err)
	}
}
