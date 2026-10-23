package checkpoint

import (
	"errors"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// chmodUnopenable sets the mode of name in d, which st (just looked at
// without following) says is a directory or a regular file its owner
// cannot open for reading, with fchmodat(AT_SYMLINK_NOFOLLOW), which macOS
// honours: on a symlink swapped in meanwhile it changes the link itself,
// which is harmless. It returns false, and no error, when the entry is
// gone.
func chmodUnopenable(d *os.File, name string, st *unix.Stat_t, mode uint32) (bool, error) {
	err := unix.Fchmodat(int(d.Fd()), name, mode, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(d)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return err == nil, err
}
