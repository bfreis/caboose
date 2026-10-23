package saferm

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

// procSelfFd is where a descriptor's magic link is; a variable so a test
// can act as if /proc were missing.
var procSelfFd = "/proc/self/fd"

// openParent opens the resolved directory holding the tree, for the *at
// calls only (O_PATH: it need not be readable).
func openParent(dir string) (*os.File, error) {
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	return os.NewFile(uintptr(fd), dir), nil
}

// chmodUnreadable sets the mode of the directory name of dir, which st
// says has no owner read, without opening it for reading: it is opened
// O_PATH (no permission needed), checked to be the directory looked at, and
// changed through /proc/self/fd/N, which names exactly that object. Where
// /proc is missing, fchmodat2's AT_SYMLINK_NOFOLLOW (Linux 6.6) is used. It
// returns false, and no error, when the entry is no longer that directory.
func chmodUnreadable(dir *os.File, name, shown string, st *unix.Stat_t, mode uint32) (bool, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	runtime.KeepAlive(dir)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("saferm: %s: %w", shown, err)
	}
	f := os.NewFile(uintptr(fd), shown)
	defer f.Close()
	var now unix.Stat_t
	err = unix.Fstat(int(f.Fd()), &now)
	runtime.KeepAlive(f)
	if err != nil {
		return false, fmt.Errorf("saferm: %s: %w", shown, err)
	}
	if !sameObject(st, &now) {
		return false, nil
	}
	if _, err := os.Stat(procSelfFd); err == nil {
		err = unix.Fchmodat(unix.AT_FDCWD, procSelfFd+"/"+strconv.Itoa(int(f.Fd())), mode, 0)
		runtime.KeepAlive(f)
		if err != nil {
			return false, fmt.Errorf("saferm: %s: %w", shown, err)
		}
		return true, nil
	}
	err = unix.Fchmodat(int(dir.Fd()), name, mode, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(dir)
	if err != nil {
		return false, fmt.Errorf("saferm: %s: no owner read, and without /proc its mode cannot be changed safely: %w", shown, err)
	}
	return true, nil
}

// fixFile: on Linux a regular file needs nothing for its removal (the
// parent's write permission is what counts).
func fixFile(dir *os.File, name, shown string, st *unix.Stat_t) error { return nil }

// clearFlags: Linux has no flags that removal must clear here.
func clearFlags(f *os.File, shown string, st *unix.Stat_t) error { return nil }
