package checkpoint

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestCopyRaces changes the source between the walker's look at an entry
// and its open, as a sandbox writing the tree could: nothing outside the
// source is ever read through a swapped-in symlink, no fifo is read, a
// replaced or vanished entry is skipped, and nothing is written outside dst.
func TestCopyRaces(t *testing.T) {
	top := tempTree(t)
	src := filepath.Join(top, "src")
	mustWrite(t, filepath.Join(top, "outside.txt"), "SECRET", 0o644)
	mustWrite(t, filepath.Join(top, "outdir", "secret"), "SECRET", 0o644)
	for _, f := range []string{"tolink", "tofifo", "removed", "replaced", "movedlink", "keep"} {
		mustWrite(t, filepath.Join(src, f), "orig "+f, 0o644)
	}
	if err := os.Mkdir(filepath.Join(src, "dirtolink"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, "dirmovedlink", "inner"), "inner", 0o644)
	mustWrite(t, filepath.Join(src, "renamed", "inner"), "inner", 0o644)
	mustWrite(t, filepath.Join(src, "dirreplaced", "inner"), "inner", 0o644)

	at := func(n string) string { return filepath.Join(src, n) }
	must := func(err error) {
		if err != nil {
			t.Error(err)
		}
	}
	swaps := map[string]func(){
		// A file replaced by a symlink to a file outside.
		"tolink": func() {
			must(os.Remove(at("tolink")))
			must(os.Symlink(filepath.Join(top, "outside.txt"), at("tolink")))
		},
		// A file moved away and a symlink to it put in its place: the same
		// inode, so only O_NOFOLLOW keeps it from being followed.
		"movedlink": func() {
			must(os.Rename(at("movedlink"), filepath.Join(top, "moved-file")))
			must(os.Symlink(filepath.Join(top, "moved-file"), at("movedlink")))
		},
		// A directory replaced by a symlink to a directory outside.
		"dirtolink": func() {
			must(os.Rename(at("dirtolink"), filepath.Join(top, "gone-dir")))
			must(os.Symlink(filepath.Join(top, "outdir"), at("dirtolink")))
		},
		// A directory moved away and a symlink to it put in its place.
		"dirmovedlink": func() {
			must(os.Rename(at("dirmovedlink"), filepath.Join(top, "moved-dir")))
			must(os.Symlink(filepath.Join(top, "moved-dir"), at("dirmovedlink")))
		},
		// A file replaced by a fifo, which must never be read.
		"tofifo": func() { must(os.Remove(at("tofifo"))); must(unix.Mkfifo(at("tofifo"), 0o644)) },
		// A file removed.
		"removed": func() { must(os.Remove(at("removed"))) },
		// A file replaced by another file (another inode). The new file is
		// written while the old one still exists and renamed over it:
		// removing the old one first lets a filesystem such as ext4 hand
		// its inode number straight to the new one, which no identity
		// check (device, inode, type) can tell from the original.
		"replaced": func() {
			mustWrite(t, filepath.Join(top, "replacement"), "NEW!", 0o644)
			must(os.Rename(filepath.Join(top, "replacement"), at("replaced")))
		},
		// A directory replaced by another directory (another inode).
		"dirreplaced": func() {
			must(os.Rename(at("dirreplaced"), filepath.Join(top, "old-dir")))
			mustWrite(t, filepath.Join(at("dirreplaced"), "new"), "NEW!", 0o644)
		},
		// A directory renamed away.
		"renamed": func() { must(os.Rename(at("renamed"), filepath.Join(top, "away"))) },
	}
	topBefore := []string{"away", "gone-dir", "moved-dir", "moved-file", "old-dir", "outdir", "outside.txt", "src"}
	outdirBefore := snapshot(t, filepath.Join(top, "outdir"))

	root, dst, sentinel := dstFor(t)
	h := &Hooks{BeforeOpen: func(rel string) {
		if f, ok := swaps[rel]; ok {
			f()
		}
	}}
	r, err := copyT(t, context.Background(), rootSource(top, "src"), dst, Options{Root: root, NoWhole: true, NoReflink: true, Hooks: h})
	if err != nil {
		t.Fatal(err)
	}
	for rel := range swaps {
		fi, err := os.Lstat(filepath.Join(dst, rel))
		if err == nil {
			t.Errorf("%s: in the copy as %v", rel, fi.Mode())
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Error(err)
		}
		if !slices.Contains(r.SkippedExamples, rel) {
			t.Errorf("%s not among the skipped %v", rel, r.SkippedExamples)
		}
	}
	if r.Skipped != int64(len(swaps)) {
		t.Errorf("skipped %d, want %d", r.Skipped, len(swaps))
	}
	if b, err := os.ReadFile(filepath.Join(dst, "keep")); err != nil || string(b) != "orig keep" {
		t.Errorf("keep: %q %v", b, err)
	}
	// Nothing of the outside was copied, nothing written outside dst.
	_ = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "SECRET") {
				t.Errorf("%s holds the outside's content", p)
			}
		}
		return nil
	})
	assertEmpty(t, sentinel)
	des, err := os.ReadDir(top)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	if !slices.Equal(names, topBefore) {
		t.Errorf("top holds %v, want %v", names, topBefore)
	}
	after := snapshot(t, filepath.Join(top, "outdir"))
	for p, b := range outdirBefore {
		if after[p] != b {
			t.Errorf("%s changed: %+v, was %+v", p, after[p], b)
		}
	}
	if len(after) != len(outdirBefore) {
		t.Error("outdir changed")
	}
}
