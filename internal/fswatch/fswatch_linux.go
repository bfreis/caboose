package fswatch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// inotify watches directories, not trees: a watch goes on every directory
// under the roots, and on each new one as it appears.
const watchMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MODIFY | unix.IN_ATTRIB |
	unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF

type inotify struct {
	w  *Watcher
	fd int
	f  *os.File

	mu   sync.Mutex
	dirs map[int32]string // watch descriptor -> directory
}

func start(w *Watcher) (func(), error) {
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		return nil, err
	}
	// A non-blocking fd goes through the runtime's poller, so Close
	// wakes the read below.
	in := &inotify{w: w, fd: fd, f: os.NewFile(uintptr(fd), "inotify"), dirs: map[int32]string{}}
	for _, r := range w.roots {
		if err := in.addTree(r.phys, false); err != nil {
			in.f.Close()
			return nil, err
		}
	}
	go in.read()
	return func() { in.f.Close() }, nil
}

// addTree watches dir and every directory under it. report also records
// each path found, for a directory that appeared after its parent was
// read: what was made in it before its watch landed is otherwise missed.
func (in *inotify) addTree(dir string, report bool) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if in.w.isClosed() {
			// Its fd may be another file's by now.
			return filepath.SkipAll
		}
		if err != nil {
			if p == dir {
				return err
			}
			return nil
		}
		if report && p != dir {
			in.w.add(p)
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		wd, err := unix.InotifyAddWatch(in.fd, p, watchMask|unix.IN_ONLYDIR|unix.IN_DONT_FOLLOW)
		if err != nil {
			if errors.Is(err, unix.ENOSPC) {
				return errors.New("fswatch: out of inotify watches (fs.inotify.max_user_watches)")
			}
			return nil
		}
		in.mu.Lock()
		in.dirs[int32(wd)] = p
		in.mu.Unlock()
		return nil
	})
}

func (in *inotify) read() {
	buf := make([]byte, 64<<10)
	for {
		n, err := in.f.Read(buf)
		if err != nil {
			return
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			nameAt := off + unix.SizeofInotifyEvent
			off = nameAt + int(ev.Len)
			if off > n {
				break
			}
			in.handle(ev, buf[nameAt:off])
		}
	}
}

func (in *inotify) handle(ev *unix.InotifyEvent, raw []byte) {
	if ev.Mask&unix.IN_Q_OVERFLOW != 0 {
		in.w.lost()
		return
	}
	in.mu.Lock()
	dir, ok := in.dirs[ev.Wd]
	if ev.Mask&unix.IN_IGNORED != 0 {
		delete(in.dirs, ev.Wd)
	}
	in.mu.Unlock()
	if !ok {
		return
	}
	name := string(raw)
	for i := 0; i < len(name); i++ {
		if name[i] == 0 {
			name = name[:i]
			break
		}
	}
	p := dir
	if name != "" {
		p = filepath.Join(dir, name)
	}
	in.w.add(p)
	if ev.Mask&unix.IN_ISDIR != 0 && ev.Mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 && name != ".git" {
		if err := in.addTree(p, true); err != nil && !errors.Is(err, fs.ErrNotExist) {
			in.w.lost()
		}
	}
}

func (w *Watcher) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}
