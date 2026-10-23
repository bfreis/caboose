package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/saferm"
	"golang.org/x/sys/unix"
)

// What a source's top is.
const (
	SourceRoot = "root" // a writable part of a root
	SourceHome = "home" // a kept path of the sandbox's home, in the data dir
)

// Source is what a checkpoint copies: a directory the host owns (Top),
// trusted as given, and a path below it (Rel), reached without following
// anything, since the sandbox can write it.
type Source struct {
	Path string // the configured entry, as written ("/work/dev/x", "~/.claude")
	Kind string // SourceRoot or SourceHome
	Top  string // the physical host directory: a root's host path, or the data dir
	Rel  string // slash-separated and clean, below Top; "" is Top itself (never for a home source)
	File bool   // the source is a single regular file
}

// Host is the source's host path (for meta.json and messages; never opened).
func (s Source) Host() string {
	if s.Rel == "" {
		return s.Top
	}
	return filepath.Join(s.Top, filepath.FromSlash(s.Rel))
}

// Method is how a checkpoint was copied: MethodClonefile, MethodReflink or
// MethodCopy.
type Method = string

// DefaultMaxEntries bounds what Copy walks when Options.MaxEntries is 0.
const DefaultMaxEntries = 5_000_000

// maxDepth is how deep the walker descends; deeper directories are skipped.
const maxDepth = 256

// maxSkippedExamples is how many skipped paths a Result names.
const maxSkippedExamples = 20

// How often the walker checks free space: every this many bytes copied,
// and every this many entries (variables for tests only).
var (
	spaceEveryBytes   int64 = 256 << 20
	spaceEveryEntries int64 = 10_000
)

var (
	// ErrSourceSymlink is returned when the way to a source runs through a
	// symlink: the source is not copied.
	ErrSourceSymlink = errors.New("checkpoint: the path runs through a symlink")
	// ErrSourceMissing is returned when a source is not there, or is not
	// a directory (a regular file, for a file source).
	ErrSourceMissing = errors.New("checkpoint: the path is missing")
	// ErrTooLarge is returned when a tree holds more than MaxEntries.
	ErrTooLarge = errors.New("checkpoint: the tree is too large for the copy method")
	// ErrDiskFull is returned when free space falls below the hard floor,
	// or a write fails for want of space.
	ErrDiskFull = errors.New("checkpoint: the disk is nearly full")
)

// testHookSourceOpened, when set (by tests only), runs once a source was
// opened, before its parent is checked to still hold it.
var testHookSourceOpened func()

// errNoWholeClone says the platform has no whole-tree clone.
var errNoWholeClone = errors.New("checkpoint: no whole-tree clone here")

// Hooks are for tests only: they run between the walker's steps so a test
// can change the source under it, as a sandbox could.
type Hooks struct {
	// BeforeOpen runs after an entry was looked at and before it is opened.
	BeforeOpen func(rel string)
	// BeforeWrite runs before an entry is made in the destination; an
	// error it returns is taken as the write's.
	BeforeWrite func(rel string) error
	// Copied runs after each chunk of a file's data is copied, with the
	// bytes copied so far; an error it returns is taken as the write's.
	Copied func(rel string, n int64) error
	// Dev, when set, gives the device the walker sees for an entry.
	Dev func(rel string, dev uint64) uint64
}

// Options are Copy's.
type Options struct {
	// Root is the directory dst sits in (the store, or a restore's temp
	// dir). A failed whole-tree clone's dst is removed relative to the
	// descriptor of dst's parent that Copy holds, never by this path.
	Root string
	// Prev is the previous complete checkpoint's tree of the same key, ""
	// for none; PrevStarted is when that checkpoint was started. A file
	// is linked from Prev only when unchanged and older than PrevStarted
	// less 2 s; no file is linked when PrevStarted is after Now.
	Prev        string
	PrevStarted time.Time
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	// Space is called before going on, every 256 MiB copied and every
	// 10,000 entries, and after a whole-tree clone; an error stops the copy.
	Space func() error

	NoWhole    bool // skip the whole-tree clone
	NoReflink  bool // skip per-file reflink
	MaxEntries int  // walker bound, DefaultMaxEntries when 0
	Hooks      *Hooks
}

// Result is what Copy did.
type Result struct {
	Method          Method
	Entries         int64 // entries in the copy, its top aside
	Bytes           int64 // regular files' logical size
	Linked          int64 // files hard-linked from Prev
	Skipped         int64
	SkippedExamples []string // at most 20 paths relative to the source, printable
}

// Test seams for the whole-tree clone's selection, which only macOS has.
var (
	wholeClone = cloneTree
	sameDevice = fstatSameDevice
)

// Copy makes dst, which must not exist and whose parent is in o.Root, a
// copy of src. It never follows a symlink in src, never writes outside dst,
// and never modifies src. It tries a whole-tree clone first (macOS, same
// volume), then the walker, which reflinks each file where it can, links
// it from o.Prev when unchanged, or copies it. On error dst may be left
// partly made: the caller removes it.
func Copy(ctx context.Context, src Source, dst string, o Options) (Result, error) {
	if o.Root == "" || !filepath.IsAbs(o.Root) {
		return Result{}, fmt.Errorf("checkpoint: copy root %q is not an absolute path", o.Root)
	}
	if !filepath.IsAbs(dst) || filepath.Clean(dst) != dst {
		return Result{}, fmt.Errorf("checkpoint: destination %q is not an absolute clean path", dst)
	}
	dstParent, err := openDirPath(filepath.Dir(dst))
	if err != nil {
		return Result{}, fmt.Errorf("checkpoint: %w", err)
	}
	defer dstParent.Close()
	return copyAt(ctx, src, dstParent, dst, o)
}

// copyAt is Copy into dstParent, an open directory that holds dst (whose
// path is only for messages):
// everything it makes is made relative to that descriptor.
func copyAt(ctx context.Context, src Source, dstParent *os.File, dst string, o Options) (Result, error) {
	if o.Root == "" || !filepath.IsAbs(o.Root) {
		return Result{}, fmt.Errorf("checkpoint: copy root %q is not an absolute path", o.Root)
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = DefaultMaxEntries
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	h, err := openSource(src)
	if err != nil {
		return Result{}, err
	}
	defer h.close()
	dstBase := filepath.Base(dst)
	var dst0 unix.Stat_t
	if err := fstatat(dstParent, dstBase, &dst0); !errors.Is(err, unix.ENOENT) {
		if err == nil {
			return Result{}, fmt.Errorf("checkpoint: %s already exists", dst)
		}
		return Result{}, fmt.Errorf("checkpoint: %w", err)
	}

	if !o.NoWhole && h.parent != nil {
		r, ok, err := tryWhole(h, src, dst, dstParent, dstBase, o)
		if err != nil || ok {
			return r, err
		}
	}

	w := newWalker(ctx, o, h)
	if err := w.run(h, src, dstParent, dstBase); err != nil {
		return Result{}, err
	}
	return w.result(), nil
}

// tryWhole makes dst a whole-tree clone of the source when the platform
// can and both are on one device. ok is false when the walker must copy;
// whatever a failed clone left at dst is then removed.
func tryWhole(h *srcHandle, src Source, dst string, dstParent *os.File, dstBase string, o Options) (r Result, ok bool, err error) {
	same, err := sameDevice(h.parent, dstParent)
	if err != nil || !same {
		return Result{}, false, nil
	}
	err = wholeClone(h.parent, h.base, dstParent, dstBase)
	if errors.Is(err, errNoWholeClone) {
		return Result{}, false, nil
	}
	if err == nil {
		err = checkCloned(dstParent, dstBase, src.File)
	}
	if err != nil {
		// Removed through the held parent, never by path: a restore's
		// parent may sit where a sandbox can swap a name above it.
		if rerr := saferm.RemoveTreeAt(dstParent, dstBase); rerr != nil {
			return Result{}, false, fmt.Errorf("checkpoint: removing a failed clone: %w", rerr)
		}
		if diskFull(err) {
			return Result{}, false, fmt.Errorf("%w (%v)", ErrDiskFull, err)
		}
		return Result{}, false, nil
	}
	if o.Space != nil {
		if err := o.Space(); err != nil {
			return Result{}, false, err
		}
	}
	entries, bytes, err := countTree(dstParent, dstBase, int64(o.MaxEntries))
	if err != nil {
		return Result{}, false, err
	}
	return Result{Method: MethodClonefile, Entries: entries, Bytes: bytes}, true, nil
}

// checkCloned checks the clone at name in dir is of the source's type: a
// clone of a name the sandbox swapped meanwhile is not used.
func checkCloned(dir *os.File, name string, file bool) error {
	var st unix.Stat_t
	if err := fstatat(dir, name, &st); err != nil {
		return err
	}
	want := uint32(unix.S_IFDIR)
	if file {
		want = unix.S_IFREG
	}
	if uint32(st.Mode)&unix.S_IFMT != want {
		return errors.New("checkpoint: the clone is not of the source's type")
	}
	return nil
}

// countTree counts the entries (its top aside) and regular files' bytes of
// the tree at name in dir, without following anything. It is for a copy
// in the store, which no sandbox reaches, so it races nothing.
func countTree(dir *os.File, name string, max int64) (entries, bytes int64, err error) {
	var st unix.Stat_t
	if err := fstatat(dir, name, &st); err != nil {
		return 0, 0, fmt.Errorf("checkpoint: %w", err)
	}
	if uint32(st.Mode)&unix.S_IFMT == unix.S_IFREG {
		return 1, st.Size, nil
	}
	c := counter{max: max}
	f, err := openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("checkpoint: %w", err)
	}
	defer f.Close()
	if err := c.dir(f, 0); err != nil {
		return 0, 0, err
	}
	return c.entries, c.bytes, nil
}

// maxCountDepth bounds countTree's descent (one open directory per level).
const maxCountDepth = 4096

type counter struct {
	max, entries, bytes int64
}

func (c *counter) dir(d *os.File, depth int) error {
	for {
		names, rerr := d.Readdirnames(1024)
		for _, n := range names {
			if n == "." || n == ".." {
				continue
			}
			c.entries++
			if c.entries > c.max {
				return ErrTooLarge
			}
			var st unix.Stat_t
			if err := fstatat(d, n, &st); err != nil {
				continue
			}
			switch uint32(st.Mode) & unix.S_IFMT {
			case unix.S_IFREG:
				c.bytes += st.Size
			case unix.S_IFDIR:
				if depth+1 >= maxCountDepth {
					continue
				}
				sub, err := openat(d, n, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
				if err != nil {
					continue
				}
				err = c.dir(sub, depth+1)
				sub.Close()
				if err != nil {
					return err
				}
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("checkpoint: %w", rerr)
		}
	}
}

// srcHandle is a source reached without following: the source itself, and
// the directory holding it (nil when there is none to use).
type srcHandle struct {
	parent *os.File
	base   string
	f      *os.File
	st     unix.Stat_t
}

func (h *srcHandle) close() {
	if h.parent != nil {
		h.parent.Close()
	}
	h.f.Close()
}

// checkRel refuses a Rel that is not a clean relative slash path.
func checkRel(rel string) error {
	if rel == "" {
		return nil
	}
	if path.IsAbs(rel) || path.Clean(rel) != rel || strings.IndexByte(rel, 0) >= 0 {
		return fmt.Errorf("checkpoint: bad relative path %q", rel)
	}
	for _, c := range strings.Split(rel, "/") {
		if c == "" || c == "." || c == ".." {
			return fmt.Errorf("checkpoint: bad relative path %q", rel)
		}
	}
	return nil
}

// openSource opens Top (trusted) and then each component of Rel with
// O_NOFOLLOW, so a symlink anywhere on the way is refused.
func openSource(src Source) (*srcHandle, error) {
	if src.Top == "" || !filepath.IsAbs(src.Top) {
		return nil, fmt.Errorf("checkpoint: source top %q is not an absolute path", src.Top)
	}
	if err := checkRel(src.Rel); err != nil {
		return nil, err
	}
	if src.Rel == "" && (src.Kind == SourceHome || src.File) {
		return nil, fmt.Errorf("checkpoint: source %q has no path below its top", src.Path)
	}
	top, err := openDirPath(src.Top)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
			return nil, fmt.Errorf("%w: %s", ErrSourceMissing, src.Top)
		}
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	h := &srcHandle{f: top}
	if src.Rel == "" {
		if d := filepath.Dir(src.Top); d != src.Top {
			if p, err := openDirPath(d); err == nil {
				h.parent, h.base = p, filepath.Base(src.Top)
			}
		}
	} else {
		comps := strings.Split(src.Rel, "/")
		for i, c := range comps {
			flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_DIRECTORY
			if i == len(comps)-1 && src.File {
				flags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK
			}
			next, err := openat(h.f, c, flags, 0)
			if err != nil {
				defer h.close()
				switch {
				case errors.Is(err, unix.ELOOP):
					return nil, fmt.Errorf("%w: %s", ErrSourceSymlink, src.Path)
				case errors.Is(err, unix.ENOTDIR):
					// Opened O_DIRECTORY: a symlink (on some systems) or a file.
					var st unix.Stat_t
					if fstatat(h.f, c, &st) == nil && uint32(st.Mode)&unix.S_IFMT == unix.S_IFLNK {
						return nil, fmt.Errorf("%w: %s", ErrSourceSymlink, src.Path)
					}
					return nil, fmt.Errorf("%w: %s is not a directory", ErrSourceMissing, src.Path)
				case errors.Is(err, unix.ENOENT):
					return nil, fmt.Errorf("%w: %s", ErrSourceMissing, src.Path)
				}
				return nil, fmt.Errorf("checkpoint: %s: %w", src.Path, err)
			}
			if h.parent != nil {
				h.parent.Close()
			}
			h.parent, h.base, h.f = h.f, c, next
		}
	}
	if err := fstat(h.f, &h.st); err != nil {
		h.close()
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	want := uint32(unix.S_IFDIR)
	if src.File {
		want = unix.S_IFREG
	}
	if uint32(h.st.Mode)&unix.S_IFMT != want {
		h.close()
		return nil, fmt.Errorf("%w: %s is not a %s", ErrSourceMissing, src.Path, map[bool]string{true: "regular file", false: "directory"}[src.File])
	}
	if testHookSourceOpened != nil {
		testHookSourceOpened()
	}
	if h.parent != nil {
		// The parent must still hold what was opened, or a clone by name
		// could take something else.
		var st unix.Stat_t
		if fstatat(h.parent, h.base, &st) != nil || !sameFile(&st, &h.st) {
			h.parent.Close()
			h.parent = nil
		}
	}
	return h, nil
}

// fstatSameDevice says whether two open directories are on one device.
func fstatSameDevice(a, b *os.File) (bool, error) {
	var sa, sb unix.Stat_t
	if err := fstat(a, &sa); err != nil {
		return false, err
	}
	if err := fstat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}

// diskFull says whether err is a write failing for want of space.
func diskFull(err error) bool {
	return errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) || errors.Is(err, ErrDiskFull)
}

func sameFile(a, b *unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && uint32(a.Mode)&unix.S_IFMT == uint32(b.Mode)&unix.S_IFMT
}

// openDirPath opens a host directory the host owns, by path.
func openDirPath(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

// The raw *at calls: each takes the descriptor of an *os.File that owns it,
// and keeps that file alive until the call is done, so no finalizer can
// close the descriptor under it.

func openat(d *os.File, name string, flags int, mode uint32) (*os.File, error) {
	var fd int
	var err error
	for {
		fd, err = unix.Openat(int(d.Fd()), name, flags|unix.O_CLOEXEC, mode)
		runtime.KeepAlive(d)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func fstatat(d *os.File, name string, st *unix.Stat_t) error {
	for {
		err := unix.Fstatat(int(d.Fd()), name, st, unix.AT_SYMLINK_NOFOLLOW)
		runtime.KeepAlive(d)
		if err != unix.EINTR {
			return err
		}
	}
}

func fstat(f *os.File, st *unix.Stat_t) error {
	err := unix.Fstat(int(f.Fd()), st)
	runtime.KeepAlive(f)
	return err
}

func fchmod(f *os.File, mode uint32) error {
	err := unix.Fchmod(int(f.Fd()), mode)
	runtime.KeepAlive(f)
	return err
}

func mkdirat(d *os.File, name string, mode uint32) error {
	err := unix.Mkdirat(int(d.Fd()), name, mode)
	runtime.KeepAlive(d)
	return err
}

func linkat(from *os.File, fromName string, to *os.File, toName string) error {
	err := unix.Linkat(int(from.Fd()), fromName, int(to.Fd()), toName, 0)
	runtime.KeepAlive(from)
	runtime.KeepAlive(to)
	return err
}

func readlinkat(d *os.File, name string, buf []byte) (int, error) {
	n, err := unix.Readlinkat(int(d.Fd()), name, buf)
	runtime.KeepAlive(d)
	return n, err
}

func symlinkat(target string, d *os.File, name string) error {
	err := unix.Symlinkat(target, int(d.Fd()), name)
	runtime.KeepAlive(d)
	return err
}

// setTimes sets name's (in d) access and modification times, not following.
func setTimes(d *os.File, name string, st *unix.Stat_t) error {
	a, m, _ := statTimes(st)
	err := unix.UtimesNanoAt(int(d.Fd()), name, []unix.Timespec{a, m}, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(d)
	return err
}
