package agent

import (
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// changedQueue is how many of the host's change messages wait to be
// touched; past it they are dropped, and the host's next ones still come.
const changedQueue = 64

// touchChanged works through the host's change messages until the session
// ends.
func (l *Link) touchChanged() {
	for {
		select {
		case paths := <-l.changed:
			l.mu.Lock()
			roots := l.roots
			l.mu.Unlock()
			Touch(roots, paths)
		case <-l.sess.Done():
			return
		}
	}
}

// Touch makes watchers in the sandbox see that paths changed on the host,
// without changing them: each is set to the mode it already has, which
// raises IN_ATTRIB and leaves its contents and times alone. A path that is
// gone (deleted or renamed on the host) cannot raise anything itself, so
// its directory is touched instead. Anything that is not a regular file or
// a directory, or is outside every one of roots, is left alone.
func Touch(roots []string, paths []string) {
	parents := map[string]bool{}
	for _, p := range paths {
		if !underAny(roots, p) {
			continue
		}
		if err := touch(p); errors.Is(err, unix.ENOENT) {
			parents[filepath.Dir(p)] = true
		}
	}
	for d := range parents {
		if underAny(roots, d) {
			_ = touch(d)
		}
	}
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
// it never reaches past p.
func touch(p string) error {
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		return err
	}
	if t := st.Mode & unix.S_IFMT; t != unix.S_IFREG && t != unix.S_IFDIR {
		return nil
	}
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var fst unix.Stat_t
	if err := unix.Fstat(fd, &fst); err != nil {
		return err
	}
	if fst.Dev != st.Dev || fst.Ino != st.Ino {
		return nil
	}
	return unix.Fchmod(fd, uint32(fst.Mode)&0o7777)
}
