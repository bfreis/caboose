//go:build unix

package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/bfreis/caboose/internal/proposal"
	"golang.org/x/sys/unix"
)

// maxLinkTarget is the longest symlink target copied; longer ones are
// skipped.
const maxLinkTarget = 4096

// copyChunk is how much is copied between two looks at the free space
// counters.
const copyChunk = 64 << 20

// ctimeSlack is how much older than the previous checkpoint's start a
// file's ctime must be for the file to be linked from it.
const ctimeSlack = 2e9 // ns

// walker copies a source tree on file descriptors. The source is written
// by the sandbox while it is read, so every entry is looked at without
// following (fstatat), opened with O_NOFOLLOW, and checked to be what was
// looked at (device, inode, type) before it is read; anything that
// vanished or changed in between is skipped and counted. Every destination
// operation is relative to a directory the walker made under dst.
type walker struct {
	ctx context.Context
	o   Options

	topDev  uint64
	prevCut int64 // unix ns a file's ctime must be before to be linked; 0: no linking
	reflink bool

	res       Result
	seen      int64 // entries looked at, for MaxEntries
	entryTick int64
	byteTick  int64
	reflinked int64
	copied    int64
}

func newWalker(ctx context.Context, o Options, h *srcHandle) *walker {
	w := &walker{ctx: ctx, o: o, reflink: !o.NoReflink}
	w.topDev = w.dev("", &h.st)
	if o.Prev != "" && !o.PrevStarted.IsZero() && !o.PrevStarted.After(o.Now()) {
		w.prevCut = o.PrevStarted.UnixNano() - ctimeSlack
	}
	return w
}

func (w *walker) result() Result {
	r := w.res
	r.Method = MethodCopy
	if w.reflinked > 0 && w.copied == 0 {
		r.Method = MethodReflink
	}
	return r
}

func (w *walker) dev(rel string, st *unix.Stat_t) uint64 {
	d := uint64(st.Dev)
	if w.o.Hooks != nil && w.o.Hooks.Dev != nil {
		d = w.o.Hooks.Dev(rel, d)
	}
	return d
}

func (w *walker) beforeOpen(rel string) {
	if w.o.Hooks != nil && w.o.Hooks.BeforeOpen != nil {
		w.o.Hooks.BeforeOpen(rel)
	}
}

func (w *walker) beforeWrite(rel string) error {
	if w.o.Hooks != nil && w.o.Hooks.BeforeWrite != nil {
		return w.o.Hooks.BeforeWrite(rel)
	}
	return nil
}

func (w *walker) skip(rel string) {
	w.res.Skipped++
	if len(w.res.SkippedExamples) < maxSkippedExamples {
		w.res.SkippedExamples = append(w.res.SkippedExamples, proposal.Printable(rel))
	}
}

func (w *walker) space() error {
	if w.o.Space == nil {
		return nil
	}
	return w.o.Space()
}

// writeErr turns a failed write in the destination into the error Copy
// returns: ErrDiskFull for want of space.
func writeErr(rel string, err error) error {
	if diskFull(err) {
		return fmt.Errorf("%w (%s: %v)", ErrDiskFull, rel, err)
	}
	return fmt.Errorf("checkpoint: %s: %w", rel, err)
}

// gone says whether an error from looking at or opening a source entry
// means it vanished or changed meanwhile, or cannot be read: skipped.
func gone(err error) bool {
	for _, e := range []error{unix.ENOENT, unix.ENOTDIR, unix.ELOOP, unix.EACCES, unix.EPERM, unix.ENXIO, unix.EAGAIN} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// run copies the opened source to dstBase in dstParent.
func (w *walker) run(h *srcHandle, src Source, dstParent *os.File, dstBase string) error {
	if err := w.space(); err != nil {
		return err
	}
	var prevDir *os.File
	prevName := ""
	if w.prevCut != 0 {
		if src.File {
			if d, err := openDirPath(filepath.Dir(w.o.Prev)); err == nil {
				prevDir, prevName = d, filepath.Base(w.o.Prev)
			}
		} else if d, err := openDirPath(w.o.Prev); err == nil {
			prevDir = d
		}
		if prevDir != nil {
			defer prevDir.Close()
		}
	}
	if src.File {
		w.seen++
		return w.copyFile(h.f, &h.st, dstParent, dstBase, prevDir, prevName, ".")
	}
	if err := w.beforeWrite("."); err != nil {
		return writeErr(".", err)
	}
	if err := mkdirat(dstParent, dstBase, 0o700); err != nil {
		return writeErr(".", err)
	}
	d, err := openat(dstParent, dstBase, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	defer d.Close()
	if err := w.dir(h.f, d, prevDir, "", 0); err != nil {
		return err
	}
	return finishDir(dstParent, dstBase, d, &h.st)
}

// finishDir gives a copied directory, once filled, its source's mode (the
// permission bits only) and times.
func finishDir(parent *os.File, name string, d *os.File, st *unix.Stat_t) error {
	if err := fchmod(d, uint32(st.Mode)&0o777); err != nil {
		return writeErr(name, err)
	}
	if err := setTimes(parent, name, st); err != nil {
		return writeErr(name, err)
	}
	return nil
}

// dir copies the entries of src (at rel) into dst; prev is the same
// directory in the previous checkpoint, or nil.
func (w *walker) dir(src, dst, prev *os.File, rel string, depth int) error {
	for {
		names, rerr := src.Readdirnames(1024)
		for _, name := range names {
			if name == "." || name == ".." {
				continue
			}
			if err := w.entry(src, dst, prev, rel, name, depth); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("checkpoint: reading %s: %w", relOrDot(rel), rerr)
		}
	}
}

func relOrDot(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

func (w *walker) entry(src, dst, prev *os.File, rel, name string, depth int) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.seen++
	if w.seen > int64(w.o.MaxEntries) {
		return fmt.Errorf("%w (over %d entries)", ErrTooLarge, w.o.MaxEntries)
	}
	if w.entryTick++; w.entryTick >= spaceEveryEntries {
		w.entryTick = 0
		if err := w.space(); err != nil {
			return err
		}
	}
	r := path.Join(rel, name)
	var st unix.Stat_t
	if err := fstatat(src, name, &st); err != nil {
		if gone(err) {
			w.skip(r)
			return nil
		}
		return fmt.Errorf("checkpoint: %s: %w", r, err)
	}
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFDIR:
		return w.subdir(src, dst, prev, r, name, &st, depth)
	case unix.S_IFREG:
		return w.file(src, dst, prev, r, name, &st)
	case unix.S_IFLNK:
		return w.symlink(src, dst, r, name)
	default: // fifo, socket, device
		w.skip(r)
		return nil
	}
}

func (w *walker) subdir(src, dst, prev *os.File, r, name string, looked *unix.Stat_t, depth int) error {
	if w.dev(r, looked) != w.topDev || depth+1 > maxDepth {
		w.skip(r)
		return nil
	}
	w.beforeOpen(r)
	sub, err := openat(src, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if gone(err) {
			w.skip(r)
			return nil
		}
		return fmt.Errorf("checkpoint: %s: %w", r, err)
	}
	defer sub.Close()
	var st unix.Stat_t
	if err := fstat(sub, &st); err != nil || !sameFile(&st, looked) || w.dev(r, &st) != w.topDev {
		w.skip(r)
		return nil
	}
	if err := w.beforeWrite(r); err != nil {
		return writeErr(r, err)
	}
	if err := mkdirat(dst, name, 0o700); err != nil {
		return writeErr(r, err)
	}
	dsub, err := openat(dst, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("checkpoint: %s: %w", r, err)
	}
	defer dsub.Close()
	var psub *os.File
	if prev != nil {
		if p, err := openat(prev, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0); err == nil {
			psub = p
			defer psub.Close()
		}
	}
	w.res.Entries++
	if err := w.dir(sub, dsub, psub, r, depth+1); err != nil {
		return err
	}
	return finishDir(dst, name, dsub, &st)
}

func (w *walker) file(src, dst, prev *os.File, r, name string, looked *unix.Stat_t) error {
	w.beforeOpen(r)
	f, err := openat(src, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if gone(err) {
			w.skip(r)
			return nil
		}
		return fmt.Errorf("checkpoint: %s: %w", r, err)
	}
	defer f.Close()
	var st unix.Stat_t
	if err := fstat(f, &st); err != nil || !sameFile(&st, looked) {
		w.skip(r)
		return nil
	}
	return w.copyFile(f, &st, dst, name, prev, name, r)
}

// copyFile puts a copy of the open regular file f (st) at name in dst:
// a link to prevName in prevDir when that is the same unchanged file,
// else a reflink, else a copy.
func (w *walker) copyFile(f *os.File, st *unix.Stat_t, dst *os.File, name string, prevDir *os.File, prevName, r string) error {
	if err := w.beforeWrite(r); err != nil {
		return writeErr(r, err)
	}
	if w.linkPrev(st, prevDir, prevName, dst, name) {
		w.res.Entries++
		w.res.Bytes += st.Size
		w.res.Linked++
		return nil
	}
	var out *os.File
	cloned := false
	if w.reflink {
		o, err := reflinkInto(f, dst, name)
		switch {
		case err == nil:
			out, cloned = o, true
		case diskFull(err):
			if o != nil {
				o.Close()
			}
			return writeErr(r, err)
		default:
			if reflinkUnsupported(err) {
				w.reflink = false
			}
			out = o // linux: made, still empty
		}
	}
	if out == nil {
		o, err := openat(dst, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			return writeErr(r, err)
		}
		out = o
	}
	defer out.Close()
	if cloned {
		w.reflinked++
	} else {
		if err := w.copyData(out, f, r); err != nil {
			return err
		}
		w.copied++
	}
	if err := fchmod(out, uint32(st.Mode)&0o777); err != nil {
		return writeErr(r, err)
	}
	if err := setTimes(dst, name, st); err != nil {
		return writeErr(r, err)
	}
	w.res.Entries++
	w.res.Bytes += st.Size
	return nil
}

// copyData copies f's contents into out, checking free space as it goes.
func (w *walker) copyData(out, f *os.File, r string) error {
	var total int64
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		n, err := io.CopyN(out, f, copyChunk)
		total += n
		if err == nil || err == io.EOF {
			if w.o.Hooks != nil && w.o.Hooks.Copied != nil {
				if herr := w.o.Hooks.Copied(r, total); herr != nil {
					err = herr
				}
			}
		}
		w.byteTick += n
		if w.byteTick >= spaceEveryBytes {
			w.byteTick = 0
			if serr := w.space(); serr != nil {
				return serr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if diskFull(err) {
				return writeErr(r, err)
			}
			return fmt.Errorf("checkpoint: %s: %w", r, err)
		}
	}
}

// linkPrev hard-links prevName in prevDir (the previous checkpoint, in the
// store) to name in dst when it is the same unchanged file: a regular
// file of the same size, modification time and permission bits, and the
// source's ctime (which no user process can set) is older than the
// previous checkpoint's start less 2 s. Links only ever join two
// checkpoints, never a checkpoint and the source.
func (w *walker) linkPrev(st *unix.Stat_t, prevDir *os.File, prevName string, dst *os.File, name string) bool {
	if w.prevCut == 0 || prevDir == nil {
		return false
	}
	var ps unix.Stat_t
	if fstatat(prevDir, prevName, &ps) != nil || uint32(ps.Mode)&unix.S_IFMT != unix.S_IFREG {
		return false
	}
	_, m, c := statTimes(st)
	_, pm, _ := statTimes(&ps)
	if ps.Size != st.Size || pm != m || uint32(ps.Mode)&0o777 != uint32(st.Mode)&0o777 {
		return false
	}
	if c.Nano() >= w.prevCut {
		return false
	}
	return linkat(prevDir, prevName, dst, name) == nil
}

func (w *walker) symlink(src, dst *os.File, r, name string) error {
	buf := make([]byte, maxLinkTarget+1)
	n, err := readlinkat(src, name, buf)
	if err != nil {
		if gone(err) || errors.Is(err, unix.EINVAL) { // EINVAL: no longer a symlink
			w.skip(r)
			return nil
		}
		return fmt.Errorf("checkpoint: %s: %w", r, err)
	}
	if n > maxLinkTarget {
		w.skip(r)
		return nil
	}
	if err := w.beforeWrite(r); err != nil {
		return writeErr(r, err)
	}
	if err := symlinkat(string(buf[:n]), dst, name); err != nil {
		return writeErr(r, err)
	}
	var st unix.Stat_t
	if fstatat(src, name, &st) == nil && uint32(st.Mode)&unix.S_IFMT == unix.S_IFLNK {
		_ = setTimes(dst, name, &st) // best effort: a link's times matter little
	}
	w.res.Entries++
	return nil
}

// reflinkUnsupported says whether a failed reflink means the filesystem
// cannot clone, so the rest of the checkpoint does not try.
func reflinkUnsupported(err error) bool {
	for _, e := range []error{unix.EOPNOTSUPP, unix.ENOTSUP, unix.EXDEV, unix.EINVAL, unix.ENOTTY, errNoReflink} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// errNoReflink says the platform has no per-file clone.
var errNoReflink = errors.New("checkpoint: no reflink here")
