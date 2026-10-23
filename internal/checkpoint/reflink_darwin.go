package checkpoint

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// reflinkInto makes name in dst a clone of the open file src
// (fclonefileat, which clones what the descriptor holds, never a name the
// sandbox could swap), and opens it. On failure nothing is left at name.
func reflinkInto(src, dst *os.File, name string) (*os.File, error) {
	err := unix.Fclonefileat(int(src.Fd()), int(dst.Fd()), name, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY)
	runtime.KeepAlive(src)
	runtime.KeepAlive(dst)
	if err != nil {
		return nil, err
	}
	// A clone keeps the source's mode, which may deny its owner reading
	// (0044): give the new entry, the walker's own in a directory it made,
	// owner read and write by name before opening it (macOS honours
	// AT_SYMLINK_NOFOLLOW, and the clone of an open file is no symlink).
	// The caller sets the real mode through the descriptor after.
	err = unix.Fchmodat(int(dst.Fd()), name, 0o600, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(dst)
	if err != nil {
		return nil, err
	}
	return openat(dst, name, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
}
