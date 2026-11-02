package diskusage

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeUnshared reports each file's size divided by div as its unshared
// size, counting its calls; a name in unsupported has none.
type fakeUnshared struct {
	div         int64
	calls       int
	unsupported map[string]bool
	// tick, when set, runs on every call: the tests' clock moves there.
	tick func()
}

func (f *fakeUnshared) UnsharedSize(dirfd int, name string) (int64, error) {
	f.calls++
	if f.tick != nil {
		f.tick()
	}
	if f.unsupported[name] {
		return 0, ErrUnsupported
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, err
	}
	return st.Size / f.div, nil
}

func write(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func blocks(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Blocks * 512
}

// A sparse file's apparent size is its length; its allocated size is only
// the blocks written.
func TestSparseFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "disk.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, 64<<10), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var w Walker
	u := walk(&w, p, nil)
	if u.Files != 1 || u.Apparent != 1<<30 {
		t.Errorf("files %d apparent %d", u.Files, u.Apparent)
	}
	if u.Allocated != blocks(t, p) || u.Allocated >= 1<<30 || u.Allocated < 64<<10 {
		t.Errorf("allocated %d, du says %d", u.Allocated, blocks(t, p))
	}
}

// A file with several hard links counts once, across walks too; a symlink
// counts as itself, never what it points to; directories count their
// blocks, not as files.
func TestTreeHardLinksSymlinks(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	write(t, filepath.Join(a, "f"), 10000)
	write(t, filepath.Join(a, "sub", "g"), 3000)
	if err := os.Link(filepath.Join(a, "f"), filepath.Join(a, "sub", "f-link")); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big")
	write(t, big, 1<<20)
	if err := os.Symlink(big, filepath.Join(a, "to-big")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(a, "to-dir")); err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(dir, "b")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(a, "f"), filepath.Join(b, "f2")); err != nil {
		t.Fatal(err)
	}

	var w Walker
	u := walk(&w, a, nil)
	if u.Files != 2 || u.Apparent != 13000 {
		t.Errorf("a: files %d apparent %d, want 2 and 13000", u.Files, u.Apparent)
	}
	want := blocks(t, a) + blocks(t, filepath.Join(a, "sub")) + blocks(t, filepath.Join(a, "f")) +
		blocks(t, filepath.Join(a, "sub", "g")) + blocks(t, filepath.Join(a, "to-big")) + blocks(t, filepath.Join(a, "to-dir"))
	if u.Allocated != want {
		t.Errorf("a: allocated %d, want %d", u.Allocated, want)
	}
	// b holds only another link to a/f, already counted.
	if ub := walk(&w, b, nil); ub.Files != 0 || ub.Apparent != 0 {
		t.Errorf("b: %+v, want its link counted already", ub)
	}
	// A new walker counts it again.
	var w2 Walker
	if ub := walk(&w2, b, nil); ub.Files != 1 || ub.Apparent != 10000 {
		t.Errorf("b on its own: %+v", ub)
	}
}

// skip leaves out a path and everything under it, the top path included;
// a path that is not there counts nothing.
func TestSkipAndMissing(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "keep", "x"), 100)
	write(t, filepath.Join(dir, "skip", "y"), 200)
	skipped := filepath.Join(dir, "skip")
	var w Walker
	u := walk(&w, dir, func(p string) bool { return p == skipped })
	if u.Files != 1 || u.Apparent != 100 {
		t.Errorf("%+v", u)
	}
	if u := walk(&w, dir, func(p string) bool { return p == dir }); u != (Usage{}) {
		t.Errorf("top skipped: %+v", u)
	}
	if u := walk(&w, filepath.Join(dir, "nope", "deeper"), nil); u != (Usage{}) {
		t.Errorf("missing: %+v", u)
	}
}

// Unshared sizes are summed over the files measured, once per file;
// a file the filesystem has none for is counted, unmeasured.
func TestPrivateSum(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a"), 4000)
	write(t, filepath.Join(dir, "b"), 8000)
	write(t, filepath.Join(dir, "c"), 1000)
	if err := os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "a-link")); err != nil {
		t.Fatal(err)
	}
	p := &fakeUnshared{div: 4, unsupported: map[string]bool{"c": true}}
	w := Walker{Unshared: p}
	u := walk(&w, dir, nil)
	if u.Files != 3 || u.Unshared != 1000+2000 || u.UnsharedFiles != 2 || u.UnsharedComplete() {
		t.Errorf("%+v", u)
	}
	if p.calls != 3 || w.Unsupported() != 1 {
		t.Errorf("calls %d, unsupported %d", p.calls, w.Unsupported())
	}
}

// Past the budget files are still counted, but no longer measured; the
// walker says so, and how long it took.
func TestPrivateBudget(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		write(t, filepath.Join(dir, n), 1000)
	}
	now := time.Unix(1000, 0)
	p := &fakeUnshared{div: 1, tick: func() { now = now.Add(time.Second) }}
	w := Walker{Unshared: p, Budget: 2 * time.Second, Now: func() time.Time { return now }}
	u := walk(&w, dir, nil)
	if u.Files != 5 || u.UnsharedFiles != 2 || u.Unshared != 2000 {
		t.Errorf("%+v", u)
	}
	if !w.OverBudget() || p.calls != 2 || w.Elapsed() != 2*time.Second {
		t.Errorf("over %v calls %d elapsed %v", w.OverBudget(), p.calls, w.Elapsed())
	}
	// Within the budget, nothing is over.
	now = time.Unix(1000, 0)
	w2 := Walker{Unshared: &fakeUnshared{div: 1}, Budget: time.Hour, Now: func() time.Time { return now }}
	if u := walk(&w2, dir, nil); !u.UnsharedComplete() || w2.OverBudget() {
		t.Errorf("%+v over %v", u, w2.OverBudget())
	}
}

// A tree deeper than maxDepth counts what is above the bound, and the
// directory past it as unreadable.
func TestDepthBound(t *testing.T) {
	dir := t.TempDir()
	p := dir
	for i := 0; i <= maxDepth; i++ {
		p = filepath.Join(p, "d")
	}
	write(t, filepath.Join(p, "deepest"), 10)
	write(t, filepath.Join(dir, "d", "shallow"), 10)
	var w Walker
	u := walk(&w, dir, nil)
	if u.Unreadable != 1 || u.Files != 1 {
		t.Errorf("%+v", u)
	}
}

// A directory that cannot be listed counts as unreadable, not as empty
// and silent.
func TestUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	write(t, filepath.Join(locked, "x"), 10)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	var w Walker
	if u := walk(&w, dir, nil); u.Unreadable != 1 {
		t.Errorf("%+v", u)
	}
}

// parseReply takes the private size only from a reply whose returned
// attributes say it is there.
func TestParseReply(t *testing.T) {
	reply := func(length, returned uint32, size uint64) []byte {
		b := make([]byte, replySize+8)
		binary.LittleEndian.PutUint32(b, length)
		binary.LittleEndian.PutUint32(b[returnedFork:], returned)
		binary.LittleEndian.PutUint64(b[privateSizeOff:], size)
		return b
	}
	if n, err := parseReply(reply(replySize, attrCmnExtPrivateSize, 12345)); err != nil || n != 12345 {
		t.Errorf("returned: %d %v", n, err)
	}
	if _, err := parseReply(reply(replySize, 0, 12345)); !errors.Is(err, ErrUnsupported) {
		t.Errorf("not returned: %v", err)
	}
	if _, err := parseReply(reply(4+4*attrBitMapCount, attrCmnExtPrivateSize, 12345)); !errors.Is(err, ErrUnsupported) {
		t.Errorf("short: %v", err)
	}
	if _, err := parseReply(make([]byte, 8)); !errors.Is(err, ErrUnsupported) {
		t.Errorf("tiny buffer: %v", err)
	}
}

// walk is one walk of w, into a Usage of its own.
func walk(w *Walker, path string, skip func(string) bool) Usage {
	var u Usage
	w.Walk(&u, path, skip)
	return u
}

// A file with a link the walk never meets is counted, but its unshared
// size is not: deleting what was walked would leave it. Once a later walk
// meets the last link, the size goes to the Usage the file was counted
// in.
func TestLinksOutside(t *testing.T) {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in"), filepath.Join(dir, "out")
	write(t, filepath.Join(in, "linked"), 8000)
	write(t, filepath.Join(in, "alone"), 4000)
	write(t, filepath.Join(out, "x"), 100)
	if err := os.Link(filepath.Join(in, "linked"), filepath.Join(out, "linked")); err != nil {
		t.Fatal(err)
	}
	w := Walker{Unshared: &fakeUnshared{div: 2}}
	var u Usage
	w.Walk(&u, in, nil)
	if u.Files != 2 || u.Apparent != 12000 || u.Unshared != 2000 || u.UnsharedFiles != 1 || u.UnsharedComplete() {
		t.Errorf("in alone: %+v", u)
	}
	if w.LinkedOutside() != 1 {
		t.Errorf("linked outside %d, want 1", w.LinkedOutside())
	}
	var u2 Usage
	w.Walk(&u2, out, nil)
	if u.Unshared != 2000+4000 || u.UnsharedFiles != 2 || !u.UnsharedComplete() {
		t.Errorf("in, once out met the last link: %+v", u)
	}
	if u2.Files != 1 || u2.Unshared != 50 || u2.UnsharedFiles != 1 {
		t.Errorf("out: %+v", u2)
	}
	if w.LinkedOutside() != 0 {
		t.Errorf("linked outside %d after both walks", w.LinkedOutside())
	}
}

// Past maxTracked files of several links, a new one is counted where it
// is met, unmeasured, and the walker says how many were.
func TestTrackedBound(t *testing.T) {
	prev := maxTracked
	maxTracked = 1
	t.Cleanup(func() { maxTracked = prev })
	dir := t.TempDir()
	for _, n := range []string{"a", "b"} {
		write(t, filepath.Join(dir, "files", n), 1000)
		if err := os.Link(filepath.Join(dir, "files", n), filepath.Join(dir, "elsewhere-"+n)); err != nil {
			t.Fatal(err)
		}
	}
	p := &fakeUnshared{div: 1}
	w := Walker{Unshared: p}
	u := walk(&w, filepath.Join(dir, "files"), nil)
	if u.Files != 2 || u.UnsharedFiles != 0 || w.Untracked() != 1 || w.LinkedOutside() != 1 || p.calls != 1 {
		t.Errorf("%+v untracked %d outside %d calls %d", u, w.Untracked(), w.LinkedOutside(), p.calls)
	}
}

// A directory is read readBatch names at a time, to the end.
func TestReadBatches(t *testing.T) {
	prev := readBatch
	readBatch = 3
	t.Cleanup(func() { readBatch = prev })
	dir := t.TempDir()
	for i := range 10 {
		write(t, filepath.Join(dir, "d", string(rune('a'+i))), 10)
	}
	var w Walker
	if u := walk(&w, dir, nil); u.Files != 10 || u.Apparent != 100 || u.Unreadable != 0 {
		t.Errorf("%+v", u)
	}
}

// An entry deleted between its directory's listing and its stat is gone,
// not unreadable: a file, and a directory with what it held.
func TestVanishedMidWalk(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "stays"), 10)
	write(t, filepath.Join(dir, "goes"), 20)
	write(t, filepath.Join(dir, "gonedir", "x"), 30)
	var w Walker
	u := walk(&w, dir, func(p string) bool {
		if b := filepath.Base(p); b == "goes" || b == "gonedir" {
			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
		}
		return false
	})
	if u.Files != 1 || u.Apparent != 10 || u.Unreadable != 0 {
		t.Errorf("%+v", u)
	}
}

// failingUnshared fails every measure with err.
type failingUnshared struct{ err error }

func (f failingUnshared) UnsharedSize(int, string) (int64, error) { return 0, f.err }

// A file whose unshared size cannot be read, for a reason other than the
// filesystem having none, is counted, unmeasured, and the walker says so;
// one with none is unsupported.
func TestUnmeasured(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "f"), 10)
	w := Walker{Unshared: failingUnshared{syscall.EIO}}
	if u := walk(&w, dir, nil); u.Files != 1 || u.UnsharedFiles != 0 || u.Unreadable != 0 || w.Unmeasured() != 1 || w.Unsupported() != 0 {
		t.Errorf("EIO: %+v unmeasured %d unsupported %d", u, w.Unmeasured(), w.Unsupported())
	}
	w = Walker{Unshared: failingUnshared{ErrUnsupported}}
	if u := walk(&w, dir, nil); u.Files != 1 || w.Unmeasured() != 0 || w.Unsupported() != 1 {
		t.Errorf("unsupported: %+v unmeasured %d unsupported %d", u, w.Unmeasured(), w.Unsupported())
	}
}

// getattrlist's errno: ENOTSUP and EINVAL are a filesystem with no
// private size, anything else an error that keeps the errno, and none at
// all an error too.
func TestCallError(t *testing.T) {
	for _, e := range []syscall.Errno{syscall.ENOTSUP, syscall.EINVAL} {
		if err := callError("c", "f", e); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%v: %v", e, err)
		}
	}
	for _, e := range []syscall.Errno{syscall.ENOENT, syscall.EACCES, syscall.EIO, 0} {
		err := callError("c", "f", e)
		if err == nil || errors.Is(err, ErrUnsupported) || e != 0 && !errors.Is(err, e) {
			t.Errorf("%v: %v", e, err)
		}
	}
}
