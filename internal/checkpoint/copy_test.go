package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// makeWritable gives every directory under dir owner rwx at cleanup, so
// t.TempDir's own removal can go through copies of read-only directories.
// Registered after the TempDir, it runs before its removal.
func makeWritable(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			if err != nil && d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
				return filepath.SkipDir
			}
			return nil
		})
		// A second pass for what the first could not enter.
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
}

// tempTree is a new temp dir, made writable again at cleanup, as its
// physical path: the temp dir may be reached through a symlink (a Mac's
// /var is /private/var), which the code resolves where it must.
func tempTree(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	makeWritable(t, d)
	return d
}

func mustWrite(t *testing.T, p, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, p string) {
	t.Helper()
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func lstat(t *testing.T, p string) unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// entryState is what a copy must never change about a source entry.
type entryState struct {
	typ, mode    uint32
	size         int64
	mtime, ctime unix.Timespec
	ino          uint64
}

// snapshot is every entry under root as entryState, by path. Where
// ctimeIsOurs says the file system keeps no ctime of this kernel's own,
// the ctimes are left out (zero): they move there with nothing changing
// the files, and so tell nothing of what the code did.
func snapshot(t *testing.T, root string) map[string]entryState {
	t.Helper()
	out := rawSnapshot(t, root)
	if !ctimeIsOurs(t, root) {
		for p, e := range out {
			e.ctime = unix.Timespec{}
			out[p] = e
		}
	}
	return out
}

// rawSnapshot is snapshot with the ctimes as the file system says them.
func rawSnapshot(t *testing.T, root string) map[string]entryState {
	t.Helper()
	out := map[string]entryState{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		_, m, c := statTimes(&st)
		out[p] = entryState{typ: uint32(st.Mode) & unix.S_IFMT, mode: uint32(st.Mode) & 0o7777, size: st.Size, mtime: m, ctime: c, ino: st.Ino}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Linux's magic numbers of the file systems ctimeIsOurs knows.
const (
	fuseMagic = 0x65735546 // FUSE, virtiofs among them
	v9fsMagic = 0x01021997 // 9p
)

// ctimeIsOurs reports whether dir's file system keeps ctimes of this
// kernel's own: not one shared with another machine through FUSE,
// virtiofs or 9p (a Mac's directory in a Linux VM, as OrbStack's /work
// is), which sets a changed file's ctime again a moment later, when the
// other side applied the change, by a delay that grows with the load.
func ctimeIsOurs(t *testing.T, dir string) bool {
	t.Helper()
	if runtime.GOOS != "linux" {
		return true
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	switch uint64(st.Type) {
	case fuseMagic, v9fsMagic:
		return false
	}
	return true
}

// settledSnapshot is rawSnapshot once what it sees has stopped changing:
// the same over settleWindow of polls. Where ctimeIsOurs says no, the
// ctimes move a moment after the last change; on any other file system
// the first polls agree. It gives up after settleMax, with what it saw
// last.
func settledSnapshot(t *testing.T, root string) map[string]entryState {
	t.Helper()
	const poll = 25 * time.Millisecond
	last, stable := rawSnapshot(t, root), time.Duration(0)
	for deadline := time.Now().Add(settleMax); stable < settleWindow && time.Now().Before(deadline); {
		time.Sleep(poll)
		now := rawSnapshot(t, root)
		if maps.Equal(now, last) {
			stable += poll
		} else {
			last, stable = now, 0
		}
	}
	if stable < settleWindow {
		t.Logf("%s did not settle in %v", root, settleMax)
	}
	return last
}

// settleWindow and settleMax bound settledSnapshot's wait.
const (
	settleWindow = 300 * time.Millisecond
	settleMax    = 10 * time.Second
)

// inodes is every inode under root, by path.
func inodes(t *testing.T, root string) map[uint64]string {
	t.Helper()
	out := map[uint64]string{}
	for p, e := range snapshot(t, root) {
		out[e.ino] = p
	}
	return out
}

// copyT runs Copy with a timeout, so a walker that blocks (on a fifo, say)
// fails the test instead of hanging it.
func copyT(t *testing.T, ctx context.Context, src Source, dst string, o Options) (Result, error) {
	t.Helper()
	type out struct {
		r   Result
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, err := Copy(ctx, src, dst, o)
		ch <- out{r, err}
	}()
	select {
	case o := <-ch:
		return o.r, o.err
	case <-time.After(30 * time.Second):
		t.Fatal("Copy did not return")
		return Result{}, nil
	}
}

// dstFor is a fresh store-like root with dst = root/tree in it, and an
// empty sentinel directory beside dst that nothing may write into.
func dstFor(t *testing.T) (root, dst, sentinel string) {
	t.Helper()
	root = tempTree(t)
	sentinel = filepath.Join(root, "sentinel")
	if err := os.Mkdir(sentinel, 0o700); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(root, TreeName), sentinel
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(des) != 0 {
		t.Fatalf("%s holds %v", dir, des)
	}
}

func rootSource(top, rel string) Source {
	return Source{Path: "/work/x/" + rel, Kind: SourceRoot, Top: top, Rel: rel}
}

func TestCopyTree(t *testing.T) {
	skipRoot(t)
	top := tempTree(t)
	src := filepath.Join(top, "src")
	old := time.Date(2020, 3, 4, 5, 6, 7, 123456789, time.UTC)
	mustWrite(t, filepath.Join(src, "a.txt"), "alpha", 0o640)
	mustWrite(t, filepath.Join(src, "sub", "deeper", "b.bin"), "\x00\x01bravo", 0o600)
	mustWrite(t, filepath.Join(top, "outside.txt"), "SECRET", 0o644)
	mustSymlink(t, filepath.Join(top, "outside.txt"), filepath.Join(src, "abs"))
	mustSymlink(t, "sub/deeper/b.bin", filepath.Join(src, "rel"))
	mustSymlink(t, "nope", filepath.Join(src, "dangling"))
	mustSymlink(t, "../outside.txt", filepath.Join(src, "out"))
	mustSymlink(t, filepath.Join(top), filepath.Join(src, "outdir"))
	if err := unix.Mkfifo(filepath.Join(src, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, "h1"), "hard", 0o644)
	if err := os.Link(filepath.Join(src, "h1"), filepath.Join(src, "h2")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, "ro", "f"), "in ro", 0o444)
	mustWrite(t, filepath.Join(src, "suid"), "#!/bin/sh\n", 0o755)
	if err := unix.Chmod(filepath.Join(src, "suid"), 0o4755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.txt", "sub/deeper/b.bin", "sub/deeper", "sub"} {
		if err := os.Chtimes(filepath.Join(src, p), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(src, "ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(src, "ro"), old, old); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, top)

	root, dst, sentinel := dstFor(t)
	r, err := copyT(t, context.Background(), rootSource(top, "src"), dst, Options{Root: root, NoWhole: true, NoReflink: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != MethodCopy {
		t.Errorf("method %q", r.Method)
	}
	// a.txt sub deeper b.bin abs rel dangling out outdir h1 h2 ro ro/f suid
	if r.Entries != 14 {
		t.Errorf("entries %d", r.Entries)
	}
	if r.Skipped != 1 || !slices.Equal(r.SkippedExamples, []string{"fifo"}) {
		t.Errorf("skipped %d %v", r.Skipped, r.SkippedExamples)
	}
	if want := int64(len("alpha") + len("\x00\x01bravo") + 2*len("hard") + len("in ro") + len("#!/bin/sh\n")); r.Bytes != want {
		t.Errorf("bytes %d, want %d", r.Bytes, want)
	}
	for p, want := range map[string]string{"a.txt": "alpha", "sub/deeper/b.bin": "\x00\x01bravo", "h1": "hard", "h2": "hard", "ro/f": "in ro"} {
		b, err := os.ReadFile(filepath.Join(dst, p))
		if err != nil || string(b) != want {
			t.Errorf("%s: %q %v", p, b, err)
		}
	}
	for p, want := range map[string]string{"abs": filepath.Join(top, "outside.txt"), "rel": "sub/deeper/b.bin", "dangling": "nope", "out": "../outside.txt", "outdir": top} {
		got, err := os.Readlink(filepath.Join(dst, p))
		if err != nil || got != want {
			t.Errorf("%s -> %q %v", p, got, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "fifo")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("fifo copied: %v", err)
	}
	if lstat(t, filepath.Join(dst, "h1")).Ino == lstat(t, filepath.Join(dst, "h2")).Ino {
		t.Error("a hard-linked pair was copied as a link")
	}
	if m := lstat(t, filepath.Join(dst, "ro")).Mode & 0o7777; m != 0o555 {
		t.Errorf("ro is %o", m)
	}
	if m := lstat(t, filepath.Join(dst, "ro", "f")).Mode & 0o7777; m != 0o444 {
		t.Errorf("ro/f is %o", m)
	}
	if m := lstat(t, filepath.Join(dst, "suid")).Mode & 0o7777; m != 0o755 {
		t.Errorf("suid is %o, want the setuid bit dropped", m)
	}
	if m := lstat(t, filepath.Join(dst, "a.txt")).Mode & 0o7777; m != 0o640 {
		t.Errorf("a.txt is %o", m)
	}
	for _, p := range []string{"a.txt", "sub/deeper/b.bin", "sub/deeper", "sub", "ro"} {
		s := lstat(t, filepath.Join(dst, p))
		_, m, _ := statTimes(&s)
		if time.Unix(m.Unix()).UTC() != old {
			t.Errorf("%s mtime %v, want %v", p, time.Unix(m.Unix()).UTC(), old)
		}
	}
	// The source, entry by entry, is as it was.
	after := snapshot(t, top)
	for p, b := range before {
		if a, ok := after[p]; !ok || a != b {
			t.Errorf("source %s changed: %+v -> %+v", p, b, a)
		}
	}
	if len(after) != len(before) {
		t.Errorf("source has %d entries, had %d", len(after), len(before))
	}
	src2 := inodes(t, top)
	for ino, p := range inodes(t, dst) {
		if q, ok := src2[ino]; ok {
			t.Errorf("%s shares its inode with the source's %s", p, q)
		}
	}
	assertEmpty(t, sentinel)
}

func TestCopySourceThroughSymlink(t *testing.T) {
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "real", "x", "f"), "x", 0o644)
	mustWrite(t, filepath.Join(top, "real", "file"), "x", 0o644)
	mustSymlink(t, "real", filepath.Join(top, "link"))
	mustSymlink(t, "file", filepath.Join(top, "real", "flink"))
	mustSymlink(t, "x", filepath.Join(top, "real", "xlink"))
	for _, s := range []Source{
		rootSource(top, "link"),
		rootSource(top, "link/x"),
		rootSource(top, "real/xlink"),
		{Path: "~/f", Kind: SourceHome, Top: top, Rel: "real/flink", File: true},
	} {
		root, dst, _ := dstFor(t)
		_, err := copyT(t, context.Background(), s, dst, Options{Root: root, NoWhole: true})
		if !errors.Is(err, ErrSourceSymlink) {
			t.Errorf("%s: %v, want ErrSourceSymlink", s.Rel, err)
		}
		if _, err := os.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: dst made", s.Rel)
		}
	}
	for _, s := range []Source{rootSource(top, "missing"), rootSource(top, "real/file"), {Path: "~/x", Kind: SourceHome, Top: top, Rel: "real/x", File: true}} {
		root, dst, _ := dstFor(t)
		if _, err := copyT(t, context.Background(), s, dst, Options{Root: root, NoWhole: true}); !errors.Is(err, ErrSourceMissing) {
			t.Errorf("%s: %v, want ErrSourceMissing", s.Rel, err)
		}
	}
	for _, rel := range []string{"/abs", "a/../b", "a//b", "./a", "..", "a/"} {
		root, dst, _ := dstFor(t)
		if _, err := copyT(t, context.Background(), rootSource(top, rel), dst, Options{Root: root, NoWhole: true}); err == nil {
			t.Errorf("rel %q accepted", rel)
		}
	}
}

func TestCopyFileSource(t *testing.T) {
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "home", ".claude.json"), `{"a":1}`, 0o600)
	root, dst, _ := dstFor(t)
	r, err := copyT(t, context.Background(), Source{Path: "~/.claude.json", Kind: SourceHome, Top: top, Rel: "home/.claude.json", File: true}, dst, Options{Root: root, NoWhole: true, NoReflink: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != `{"a":1}` {
		t.Fatalf("%q %v", b, err)
	}
	if r.Entries != 1 || r.Bytes != 7 {
		t.Errorf("%+v", r)
	}
}

func TestCopyMaxEntries(t *testing.T) {
	top := tempTree(t)
	for i := range 50 {
		mustWrite(t, filepath.Join(top, "src", fmt.Sprint(i)), "x", 0o644)
	}
	root, dst, _ := dstFor(t)
	_, err := copyT(t, context.Background(), rootSource(top, "src"), dst, Options{Root: root, NoWhole: true, MaxEntries: 10})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("%v, want ErrTooLarge", err)
	}
}

func TestCopyCancel(t *testing.T) {
	top := tempTree(t)
	for i := range 50 {
		mustWrite(t, filepath.Join(top, "src", fmt.Sprint(i)), "x", 0o644)
	}
	root, dst, _ := dstFor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	h := &Hooks{BeforeOpen: func(string) {
		if n++; n == 5 {
			cancel()
		}
	}}
	_, err := copyT(t, ctx, rootSource(top, "src"), dst, Options{Root: root, NoWhole: true, Hooks: h})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v, want context.Canceled", err)
	}
}

func TestCopyLargeDir(t *testing.T) {
	top := tempTree(t)
	const n = 2500
	for i := range n {
		mustWrite(t, filepath.Join(top, "src", fmt.Sprintf("f%04d", i)), "x", 0o644)
	}
	root, dst, _ := dstFor(t)
	r, err := copyT(t, context.Background(), rootSource(top, "src"), dst, Options{Root: root, NoWhole: true})
	if err != nil {
		t.Fatal(err)
	}
	des, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if r.Entries != n || len(des) != n {
		t.Fatalf("entries %d, %d in the copy, want %d", r.Entries, len(des), n)
	}
}

func TestCopyStaysOnDevice(t *testing.T) {
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "src", "mnt", "f"), "other fs", 0o644)
	mustWrite(t, filepath.Join(top, "src", "keep", "f"), "here", 0o644)
	root, dst, _ := dstFor(t)
	h := &Hooks{Dev: func(rel string, dev uint64) uint64 {
		if rel == "mnt" {
			return dev + 1
		}
		return dev
	}}
	r, err := copyT(t, context.Background(), rootSource(top, "src"), dst, Options{Root: root, NoWhole: true, Hooks: h})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "mnt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a directory on another device was copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "keep", "f")); err != nil {
		t.Error(err)
	}
	if r.Skipped != 1 || !slices.Contains(r.SkippedExamples, "mnt") {
		t.Errorf("%+v", r)
	}
}

// TestCopyForcedGC collects garbage between the walker's calls, so a raw
// descriptor whose owning file was not kept alive would be closed under a
// call (run with -race too).
func TestCopyForcedGC(t *testing.T) {
	top := tempTree(t)
	for i := range 30 {
		mustWrite(t, filepath.Join(top, "src", fmt.Sprintf("d%d", i%5), fmt.Sprint(i)), strings.Repeat("y", i), 0o644)
	}
	mustSymlink(t, "d0", filepath.Join(top, "src", "l"))
	root, dst, _ := dstFor(t)
	h := &Hooks{BeforeOpen: func(string) { runtime.GC() }, BeforeWrite: func(string) error { runtime.GC(); return nil }}
	r, err := copyT(t, context.Background(), rootSource(top, "src"), dst, Options{Root: root, NoWhole: true, Hooks: h})
	if err != nil {
		t.Fatal(err)
	}
	if r.Entries != 36 || r.Skipped != 0 {
		t.Fatalf("%+v", r)
	}
	for i := range 30 {
		b, err := os.ReadFile(filepath.Join(dst, fmt.Sprintf("d%d", i%5), fmt.Sprint(i)))
		if err != nil || string(b) != strings.Repeat("y", i) {
			t.Fatalf("%d: %q %v", i, b, err)
		}
	}
}

func TestCopyReflink(t *testing.T) {
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "src", "f"), "clone me", 0o644)
	mustWrite(t, filepath.Join(top, "src", "g"), "and me", 0o644)
	root, dst, _ := dstFor(t)
	r, err := copyT(t, context.Background(), rootSource(top, "src"), dst, Options{Root: root, NoWhole: true})
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{"f": "clone me", "g": "and me"} {
		if b, err := os.ReadFile(filepath.Join(dst, p)); err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", p, b, err)
		}
	}
	if r.Method != MethodReflink {
		t.Skipf("reflink not supported here (method %s)", r.Method)
	}
}

// TestLinkFromPrev: an unchanged file is linked from the previous
// checkpoint, a file rewritten with its old size and mtime is copied (its
// ctime is newer), a file whose mode changed is copied, and nothing is
// linked to the source.
func TestLinkFromPrev(t *testing.T) {
	top := tempTree(t)
	src := filepath.Join(top, "src")
	old := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, p := range []string{"same", "sub/same", "rewritten", "mode"} {
		mustWrite(t, filepath.Join(src, p), "content "+p, 0o644)
		if err := os.Chtimes(filepath.Join(src, p), old, old); err != nil {
			t.Fatal(err)
		}
	}
	root := tempTree(t)
	prev := filepath.Join(root, "prev")
	if _, err := copyT(t, context.Background(), rootSource(top, "src"), prev, Options{Root: root, NoWhole: true, NoReflink: true}); err != nil {
		t.Fatal(err)
	}
	// Every ctime the setup set, as it stays: t0 is after them all.
	settledSnapshot(t, src)
	t0 := time.Now()
	time.Sleep(30 * time.Millisecond)
	// Same size, same mtime, new content: only the ctime tells.
	mustWrite(t, filepath.Join(src, "rewritten"), "CONTENT rewritten", 0o644)
	if err := os.Chtimes(filepath.Join(src, "rewritten"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "mode"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Root: root, NoWhole: true, NoReflink: true, Prev: prev,
		PrevStarted: t0.Add(ctimeSlack + 10*time.Millisecond),
		Now:         func() time.Time { return t0.Add(time.Hour) },
	}
	next := filepath.Join(root, "next")
	r, err := copyT(t, context.Background(), rootSource(top, "src"), next, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"same": true, "sub/same": true, "rewritten": false, "mode": false}
	if !ctimeIsOurs(t, src) {
		// The file system may still set an unchanged file's ctime after
		// t0 (settledSnapshot waits only so long): the walk then copies
		// it, rightly, as it would any file whose ctime is too recent.
		for _, p := range []string{"same", "sub/same"} {
			if c := lstat(t, filepath.Join(src, p)).Ctim; c.Nano() >= opts.PrevStarted.UnixNano()-ctimeSlack {
				t.Logf("%s: its ctime moved after t0, with nothing changing it: copied", p)
				want[p] = false
			}
		}
	}
	var nlinked int64
	for p, linked := range want {
		a, b := lstat(t, filepath.Join(prev, p)), lstat(t, filepath.Join(next, p))
		if (a.Ino == b.Ino) != linked {
			t.Errorf("%s: linked %v, want %v", p, a.Ino == b.Ino, linked)
		}
		if linked {
			nlinked++
		}
	}
	if r.Linked != nlinked {
		t.Errorf("linked %d, want %d", r.Linked, nlinked)
	}
	if b, _ := os.ReadFile(filepath.Join(next, "rewritten")); string(b) != "CONTENT rewritten" {
		t.Errorf("rewritten holds %q", b)
	}
	if m := lstat(t, filepath.Join(next, "mode")).Mode & 0o777; m != 0o600 {
		t.Errorf("mode %o", m)
	}
	srcInodes := inodes(t, top)
	for ino, p := range inodes(t, next) {
		if q, ok := srcInodes[ino]; ok {
			t.Errorf("%s is linked to the source's %s", p, q)
		}
	}

	// A previous checkpoint started in the future (the clock was set back)
	// is not linked from.
	opts.PrevStarted = t0.Add(2 * time.Hour)
	r, err = copyT(t, context.Background(), rootSource(top, "src"), filepath.Join(root, "future"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Linked != 0 {
		t.Errorf("linked %d from a checkpoint started in the future", r.Linked)
	}
	// Nor without a start.
	opts.PrevStarted = time.Time{}
	if r, err = copyT(t, context.Background(), rootSource(top, "src"), filepath.Join(root, "nostart"), opts); err != nil || r.Linked != 0 {
		t.Errorf("linked %d without a start (%v)", r.Linked, err)
	}
}

// fakeWhole replaces the whole-tree clone and the device check for a test.
func fakeWhole(t *testing.T, same bool, clone func(srcParent *os.File, srcBase string, dstParent *os.File, dstBase string) error) *int {
	t.Helper()
	oldClone, oldSame := wholeClone, sameDevice
	t.Cleanup(func() { wholeClone, sameDevice = oldClone, oldSame })
	calls := new(int)
	sameDevice = func(a, b *os.File) (bool, error) { return same, nil }
	wholeClone = func(sp *os.File, sb string, dp *os.File, db string) error {
		*calls++
		return clone(sp, sb, dp, db)
	}
	return calls
}

func TestWholeCloneSelection(t *testing.T) {
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "src", "a"), "aaaa", 0o644)
	mustWrite(t, filepath.Join(top, "src", "d", "b"), "bb", 0o644)
	src := rootSource(top, "src")

	t.Run("fails then walker", func(t *testing.T) {
		calls := fakeWhole(t, true, func(_ *os.File, _ string, dp *os.File, db string) error {
			// Leave something behind, read-only, as a clone cut short may.
			p := filepath.Join(dp.Name(), db)
			mustWrite(t, filepath.Join(p, "partial", "x"), "half", 0o644)
			os.Chmod(filepath.Join(p, "partial"), 0o555)
			return unix.EXDEV
		})
		root, dst, sentinel := dstFor(t)
		r, err := copyT(t, context.Background(), src, dst, Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if *calls != 1 || r.Method == MethodClonefile {
			t.Fatalf("calls %d, method %s", *calls, r.Method)
		}
		if _, err := os.Lstat(filepath.Join(dst, "partial")); !errors.Is(err, fs.ErrNotExist) {
			t.Error("the failed clone's leftovers are in the copy")
		}
		if b, _ := os.ReadFile(filepath.Join(dst, "d", "b")); string(b) != "bb" {
			t.Error("the walker did not copy")
		}
		assertEmpty(t, sentinel)
	})
	t.Run("fails with the root swapped", func(t *testing.T) {
		// A restore's root sits where a sandbox may write: swapped for a
		// symlink while the clone runs, the failed clone's removal must
		// stay in the directory Copy holds, not follow the new name.
		root, dst, _ := dstFor(t)
		underTemp(t, root)
		victim := filepath.Join(filepath.Dir(root), "victim")
		mustWrite(t, filepath.Join(victim, TreeName, "keep"), "keep", 0o644)
		moved := root + ".moved"
		fakeWhole(t, true, func(_ *os.File, _ string, dp *os.File, db string) error {
			mustWrite(t, filepath.Join(dp.Name(), db, "partial"), "half", 0o644)
			if err := os.Rename(root, moved); err != nil {
				return err
			}
			if err := os.Symlink(victim, root); err != nil {
				return err
			}
			return unix.EXDEV
		})
		_, _ = copyT(t, context.Background(), src, dst, Options{Root: root})
		if b, err := os.ReadFile(filepath.Join(victim, TreeName, "keep")); err != nil || string(b) != "keep" {
			t.Errorf("the failed clone's removal followed the swapped root: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(moved, TreeName, "partial")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the failed clone was not removed: %v", err)
		}
	})
	t.Run("other device", func(t *testing.T) {
		calls := fakeWhole(t, false, func(*os.File, string, *os.File, string) error { return nil })
		root, dst, _ := dstFor(t)
		if _, err := copyT(t, context.Background(), src, dst, Options{Root: root}); err != nil {
			t.Fatal(err)
		}
		if *calls != 0 {
			t.Error("a whole clone across devices was tried")
		}
	})
	t.Run("NoWhole", func(t *testing.T) {
		calls := fakeWhole(t, true, func(*os.File, string, *os.File, string) error { return nil })
		root, dst, _ := dstFor(t)
		if _, err := copyT(t, context.Background(), src, dst, Options{Root: root, NoWhole: true}); err != nil {
			t.Fatal(err)
		}
		if *calls != 0 {
			t.Error("a whole clone was tried with NoWhole")
		}
	})
	t.Run("counts from the copy", func(t *testing.T) {
		// The fake clone makes a tree unlike the source: the counts must be
		// the copy's.
		fakeWhole(t, true, func(_ *os.File, sb string, dp *os.File, db string) error {
			if sb != "src" {
				return fmt.Errorf("cloning %q", sb)
			}
			p := filepath.Join(dp.Name(), db)
			for i := range 3 {
				mustWrite(t, filepath.Join(p, "x", fmt.Sprint(i)), "0123456789", 0o644)
			}
			return nil
		})
		root, dst, _ := dstFor(t)
		r, err := copyT(t, context.Background(), src, dst, Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if r.Method != MethodClonefile || r.Entries != 4 || r.Bytes != 30 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("wrong type", func(t *testing.T) {
		fakeWhole(t, true, func(_ *os.File, _ string, dp *os.File, db string) error {
			mustWrite(t, filepath.Join(dp.Name(), db), "a file", 0o644)
			return nil
		})
		root, dst, _ := dstFor(t)
		r, err := copyT(t, context.Background(), src, dst, Options{Root: root})
		if err != nil || r.Method == MethodClonefile {
			t.Fatalf("%+v %v", r, err)
		}
		if fi, err := os.Lstat(dst); err != nil || !fi.IsDir() {
			t.Fatalf("dst %v %v", fi, err)
		}
	})
	t.Run("no space", func(t *testing.T) {
		fakeWhole(t, true, func(_ *os.File, _ string, dp *os.File, db string) error {
			mustWrite(t, filepath.Join(dp.Name(), db, "x"), "half", 0o644)
			return unix.ENOSPC
		})
		root, dst, _ := dstFor(t)
		_, err := copyT(t, context.Background(), src, dst, Options{Root: root})
		if !errors.Is(err, ErrDiskFull) {
			t.Fatalf("%v, want ErrDiskFull", err)
		}
		if _, err := os.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
			t.Error("the failed clone was left")
		}
	})
}

func TestCopyRefusesExistingDst(t *testing.T) {
	top := tempTree(t)
	mustWrite(t, filepath.Join(top, "src", "a"), "a", 0o644)
	root, dst, _ := dstFor(t)
	mustWrite(t, filepath.Join(dst, "keep"), "mine", 0o644)
	if _, err := copyT(t, context.Background(), rootSource(top, "src"), dst, Options{Root: root}); err == nil {
		t.Fatal("copied over an existing dst")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "keep")); !bytes.Equal(b, []byte("mine")) {
		t.Fatal("dst changed")
	}
	if _, err := copyT(t, context.Background(), rootSource(top, "src"), filepath.Join(root, "x"), Options{}); err == nil {
		t.Fatal("copied without a root")
	}
}
