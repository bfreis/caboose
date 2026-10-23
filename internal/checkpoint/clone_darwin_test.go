//go:build darwin

package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// On a Mac, t.TempDir is on APFS, where a whole tree clones in one call.
func TestCloneTreeDarwin(t *testing.T) {
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "src", "a"), "alpha", 0o644)
	mustWrite(t, filepath.Join(top, "src", "d", "b"), "bravo", 0o600)
	mustSymlink(t, "../outside", filepath.Join(top, "src", "l"))
	if err := os.Chmod(filepath.Join(top, "src", "d"), 0o555); err != nil {
		t.Fatal(err)
	}
	sp, err := openDirPath(top)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	root, dst, _ := dstFor(t)
	dp, err := openDirPath(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dp.Close()
	if err := cloneTree(sp, "src", dp, TreeName); err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			t.Skip("clonefile is not supported here")
		}
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "d", "b")); err != nil || string(b) != "bravo" {
		t.Fatalf("%q %v", b, err)
	}
	if got, err := os.Readlink(filepath.Join(dst, "l")); err != nil || got != "../outside" {
		t.Fatalf("%q %v", got, err)
	}

	// Copy picks the whole-tree clone and counts the copy.
	root2, dst2, _ := dstFor(t)
	r, err := copyT(t, context.Background(), rootSource(top, "src"), dst2, Options{Root: root2})
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != MethodClonefile || r.Entries != 4 || r.Bytes != 10 {
		t.Fatalf("%+v", r)
	}

	// The walker's per-file clone.
	root3, dst3, _ := dstFor(t)
	r, err = copyT(t, context.Background(), rootSource(top, "src"), dst3, Options{Root: root3, NoWhole: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != MethodReflink {
		t.Errorf("method %s, want reflink", r.Method)
	}
	if b, err := os.ReadFile(filepath.Join(dst3, "a")); err != nil || string(b) != "alpha" {
		t.Fatalf("%q %v", b, err)
	}
	if m := lstat(t, filepath.Join(dst3, "d")).Mode & 0o7777; m != 0o555 {
		t.Errorf("d is %o", m)
	}
}
