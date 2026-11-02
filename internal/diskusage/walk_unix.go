//go:build unix

package diskusage

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

// dirFlags open a directory: never a symlink (O_NOFOLLOW where the
// caller adds it), and never blocking on whatever a FIFO swapped in for
// it would wait for.
const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NONBLOCK | unix.O_CLOEXEC

// Walk counts what is at path, a file or a tree, into u and into the
// walker's totals. A file with more than one link has its unshared size
// added to the Usage it was first counted in once a later walk meets its
// last link, so u must outlive every walk of w. skip, when set, is asked
// of every path met, path itself included: a path it reports is left
// out, with everything under it. A path that does not exist counts
// nothing.
func (w *Walker) Walk(u *Usage, path string, skip func(string) bool) {
	w.start()
	defer func() { w.stopped = w.now() }()
	path = filepath.Clean(path)
	if skip != nil && skip(path) {
		return
	}
	pfd, err := unix.Open(filepath.Dir(path), dirFlags, 0)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
		return
	}
	if err != nil {
		u.Unreadable++
		return
	}
	defer unix.Close(pfd)
	w.entry(u, pfd, path, filepath.Base(path), 0, skip)
}

// entry counts name, in the directory open as dirfd, whose path is path.
// An entry gone since its directory was listed is no longer there to
// count, and is not unreadable either.
func (w *Walker) entry(u *Usage, dirfd int, path, name string, depth int, skip func(string) bool) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if !errors.Is(err, unix.ENOENT) {
			u.Unreadable++
		}
		return
	}
	isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	var lf *linkedFile
	if !isDir {
		var first bool
		if first, lf = w.first(u, uint64(st.Dev), uint64(st.Ino), uint64(st.Nlink)); !first {
			return
		}
	}
	u.Allocated += int64(st.Blocks) * 512
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		u.Files++
		u.Apparent += st.Size
		// A file of several links not tracked has no unshared size that
		// can be given: it is not measured.
		if (lf != nil || st.Nlink <= 1) && w.measure() {
			n, err := w.Unshared.UnsharedSize(dirfd, name)
			switch {
			case err == nil && lf != nil:
				lf.unshared, lf.measured = n, true
			case err == nil:
				u.Unshared += n
				u.UnsharedFiles++
			case errors.Is(err, ErrUnsupported):
				w.unsupp++
			default:
				w.unmeasured++
			}
		}
	case unix.S_IFDIR:
		if depth >= maxDepth {
			u.Unreadable++
			return
		}
		// O_NOFOLLOW: a directory swapped for a symlink since the
		// fstatat is refused, never followed.
		fd, err := unix.Openat(dirfd, name, dirFlags|unix.O_NOFOLLOW, 0)
		if err != nil {
			if !errors.Is(err, unix.ENOENT) {
				u.Unreadable++
			}
			return
		}
		w.dir(u, fd, path, depth+1, skip)
	}
}

// dir counts what the directory open as fd holds, readBatch names at a
// time, and closes fd.
func (w *Walker) dir(u *Usage, fd int, path string, depth int, skip func(string) bool) {
	// Blocking again, now that it is open, so that os.File reads it as
	// the plain directory it is rather than hand it to the poller.
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		u.Unreadable++
		return
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	for {
		names, err := f.Readdirnames(readBatch)
		for _, name := range names {
			p := path + string(filepath.Separator) + name
			if skip != nil && skip(p) {
				continue
			}
			w.entry(u, fd, p, name, depth, skip)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			u.Unreadable++
			break
		}
	}
	runtime.KeepAlive(f)
}
