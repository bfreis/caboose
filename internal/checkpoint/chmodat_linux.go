package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

// procSelfFd is where a descriptor's magic link is.
const procSelfFd = "/proc/self/fd"

// chmodUnopenable sets the mode of name in d, which st (just looked at
// without following) says is a directory or a regular file its owner
// cannot open for reading. It is opened O_PATH, which needs no permission,
// checked to be the entry looked at, and changed through
// /proc/self/fd/N, which names exactly that object. Where /proc is
// missing, fchmodat2's AT_SYMLINK_NOFOLLOW (Linux 6.6) is used. It returns
// false, and no error, when the entry is no longer what was looked at.
func chmodUnopenable(d *os.File, name string, st *unix.Stat_t, mode uint32) (bool, error) {
	fd, err := unix.Openat(int(d.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	runtime.KeepAlive(d)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var now unix.Stat_t
	if err := fstat(f, &now); err != nil {
		return false, err
	}
	if !sameFile(st, &now) {
		return false, nil
	}
	if _, err := os.Stat(procSelfFd); err == nil {
		err = unix.Fchmodat(unix.AT_FDCWD, procSelfFd+"/"+strconv.Itoa(int(f.Fd())), mode, 0)
		runtime.KeepAlive(f)
		return err == nil, err
	}
	err = unix.Fchmodat(int(d.Fd()), name, mode, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(d)
	if err != nil {
		return false, fmt.Errorf("its owner cannot open it, and without /proc its mode cannot be changed safely: %w", err)
	}
	return true, nil
}
