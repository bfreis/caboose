package agent

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// changedQueue is how many of the host's change messages wait to be
// touched; past it they are dropped, and the host's next ones still come.
const changedQueue = 64

// dropLogEvery is how often, at most, the link says it dropped change
// messages: the first drop is said at once, later ones with how many
// since.
const dropLogEvery = time.Minute

// dropLog counts the change messages dropped.
type dropLog struct {
	n    int
	last time.Time
}

// note counts one dropped at now, and says whether to log it, with how
// many to say were dropped.
func (d *dropLog) note(now time.Time) (int, bool) {
	d.n++
	if !d.last.IsZero() && now.Sub(d.last) < dropLogEvery {
		return 0, false
	}
	n := d.n
	d.n, d.last = 0, now
	return n, true
}

// A touch looks its path up, and under vm that costs the Mac more than
// the lookup: Virtualization.framework's virtio-fs server keeps a file open
// for every inode the guest has looked up, until the guest's kernel
// forgets it (FUSE_FORGET), which it does when the inode leaves its
// cache: on memory pressure or a cache drop, or when a lookup finds the
// entry gone. A file the Mac deletes keeps its space as long as it is
// held.
//
// The relay sends a path again when the Mac deletes it, and its touch
// then looks it up again; but the kernel trusts the entry it has for the
// entry's timeout, which the server sets (a second), so a file deleted
// within a second of its first touch (a tool's cookie file) is found
// there still, and stays cached, and held, until memory pressure.
// Letting go of the file after the timeout would not help either: the
// kernel's FUSE client keeps an unused entry cached whatever its age
// when the server gives it a timeout (fuse_dentry_settime, fs/fuse/dir.c,
// sets no d_delete then). So under vm the agent looks each path it found
// up once more, recheckAfter later: past the timeout the kernel asks the
// server (fuse_dentry_revalidate), and for a path that is gone by then it
// drops the entry and the inode (d_invalidate in lookup_fast,
// fs/namei.c), which sends the FORGET. A path that is still there is only
// looked up, and raises no event. The paths the relay sends as gone
// without touching them (agentproto.Message's Gone: those of a directory
// it sent whole) are looked up the same way, at once, and again later if
// the guest still finds them. Under gVisor neither the server nor the
// timeout is there, and the agent does none of it.
//
// The lookups run on a goroutine of their own, so that a burst of them
// never holds up the touches, whose queue drops what does not fit.
const (
	// recheckAfter is how long after its touch a path is looked up
	// again: past the entry's timeout, with room to spare.
	recheckAfter = 2 * time.Second
	// recheckMax is the most paths waiting to be looked up again, and the
	// most gone ones waiting for their first lookup; past it the oldest
	// are dropped, and stay cached as they would have. A branch switch on
	// the Mac touches tens of thousands of paths in seconds; each waiting
	// takes some 40 bytes here besides its path's text (up to 4 KiB), and
	// r.gone holds up to as many more again; a lookup takes about 30 µs
	// (measured on a Mac: 4,096 in about 0.1 s).
	recheckMax = 1 << 16
)

// changes are one of the host's change messages: the paths to touch, and
// the gone ones only to look up.
type changes struct {
	paths, gone []string
}

// touchChanged works through the host's change messages until the session
// ends, and hands what it found to the rechecker, under vm.
func (l *Link) touchChanged() {
	for {
		select {
		case c := <-l.changed:
			l.mu.Lock()
			roots := l.roots
			l.mu.Unlock()
			found := Touch(roots, c.paths)
			if l.recheck != nil {
				var gone []string
				for _, p := range c.gone {
					if underAny(roots, p) {
						gone = append(gone, p)
					}
				}
				l.recheck.add(found, gone, time.Now())
			}
		case <-l.sess.Done():
			return
		}
	}
}

// lookUp looks p up, and says whether it found it.
func lookUp(p string) bool {
	var st unix.Stat_t
	return unix.Lstat(p, &st) == nil
}

// rechecker looks paths up again when their time comes, on its own
// goroutine (run).
type rechecker struct {
	after time.Duration
	look  func(path string) (found bool)
	wake  chan struct{}

	mu   sync.Mutex
	gone []string // to look up at once
	rc   rechecks // to look up again at their time
}

func newRechecker(after time.Duration, max int, look func(string) bool) *rechecker {
	return &rechecker{after: after, look: look, wake: make(chan struct{}, 1), rc: rechecks{max: max}}
}

// add has found looked up again after r.after, and gone at once, and
// again after r.after if the guest still finds it. It never blocks.
func (r *rechecker) add(found, gone []string, now time.Time) {
	if len(found)+len(gone) == 0 {
		return
	}
	r.mu.Lock()
	r.rc.add(found, now.Add(r.after))
	r.gone = append(r.gone, gone...)
	if over := len(r.gone) - r.rc.max; over > 0 {
		r.gone = r.gone[over:]
	}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// run looks paths up as they come due, until done is closed.
func (r *rechecker) run(done <-chan struct{}) {
	due := time.NewTimer(time.Hour)
	due.Stop()
	defer due.Stop()
	for {
		r.mu.Lock()
		gone := r.gone
		r.gone = nil
		r.mu.Unlock()
		if len(gone) > 0 {
			var still []string
			for _, p := range gone {
				if r.look(p) {
					still = append(still, p)
				}
			}
			r.mu.Lock()
			r.rc.add(still, time.Now().Add(r.after))
			r.mu.Unlock()
		}
		r.mu.Lock()
		at, ok := r.rc.next()
		r.mu.Unlock()
		if ok {
			due.Reset(time.Until(at))
		} else {
			due.Stop()
		}
		select {
		case <-r.wake:
		case <-due.C:
			r.mu.Lock()
			paths := r.rc.due(time.Now())
			r.mu.Unlock()
			for _, p := range paths {
				r.look(p)
			}
		case <-done:
			return
		}
	}
}

// rechecks are the paths to look up again, oldest first.
type rechecks struct {
	paths []recheck
	max   int
}

type recheck struct {
	path string
	at   time.Time
}

// add has paths looked up again at at, dropping the oldest past max. The
// at comes from time.Now plus a fixed delay, read by the touch loop and
// by run (which adds what it found again) under no common lock, so a call's
// may be a few microseconds before the last one's. due takes paths in
// the order added, stopping at the first not yet due, so such a path is
// looked up that much late.
func (r *rechecks) add(paths []string, at time.Time) {
	for _, p := range paths {
		r.paths = append(r.paths, recheck{path: p, at: at})
	}
	if over := len(r.paths) - r.max; over > 0 {
		r.paths = r.paths[over:]
	}
}

// due takes the paths whose time has come by now.
func (r *rechecks) due(now time.Time) []string {
	var out []string
	i := 0
	for ; i < len(r.paths) && !now.Before(r.paths[i].at); i++ {
		out = append(out, r.paths[i].path)
	}
	r.paths = r.paths[i:]
	return out
}

// next is when the oldest path is due, if any is waiting.
func (r *rechecks) next() (time.Time, bool) {
	if len(r.paths) == 0 {
		return time.Time{}, false
	}
	return r.paths[0].at, true
}

// Touch makes watchers in the sandbox see that paths changed on the host,
// without changing them: each is set to the mode it already has, which
// raises IN_ATTRIB and leaves its contents and times alone. A path that is
// gone (deleted or renamed on the host) cannot raise anything itself, so
// its directory is touched instead. Anything that is not a regular file or
// a directory, or is outside every one of roots, is left alone. It
// returns the paths of roots it found, whatever they are, and touched or
// not: each was looked up.
func Touch(roots []string, paths []string) (found []string) {
	parents := map[string]bool{}
	for _, p := range paths {
		if !underAny(roots, p) {
			continue
		}
		looked, err := touch(p)
		if looked {
			found = append(found, p)
		} else if errors.Is(err, unix.ENOENT) {
			parents[filepath.Dir(p)] = true
		}
	}
	for d := range parents {
		if underAny(roots, d) {
			_, _ = touch(d)
		}
	}
	return found
}

// underAny reports whether p, clean and absolute, is one of roots or
// inside one.
func underAny(roots []string, p string) bool {
	for _, r := range roots {
		if under(r, p) {
			return true
		}
	}
	return false
}

func under(work, p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsRune(p, 0) &&
		(p == work || strings.HasPrefix(p, work+"/"))
}

// touch sets p's mode to what it is. It goes through a file descriptor
// opened without following a symlink, checked to be the file lstat saw, so
// it never reaches past p. looked is whether lstat found p, whatever came
// after.
func touch(p string) (looked bool, err error) {
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		return false, err
	}
	if t := st.Mode & unix.S_IFMT; t != unix.S_IFREG && t != unix.S_IFDIR {
		return true, nil
	}
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return true, err
	}
	defer unix.Close(fd)
	var fst unix.Stat_t
	if err := unix.Fstat(fd, &fst); err != nil {
		return true, err
	}
	if fst.Dev != st.Dev || fst.Ino != st.Ino {
		return true, nil
	}
	return true, unix.Fchmod(fd, uint32(fst.Mode)&0o7777)
}
