package checkpoint

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// reflinkInto makes name in dst a clone of the open file src (FICLONE).
// When the clone fails, the file it made, still empty, is returned with the
// error for the caller to copy into.
func reflinkInto(src, dst *os.File, name string) (*os.File, error) {
	out, err := openat(dst, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	err = unix.IoctlFileClone(int(out.Fd()), int(src.Fd()))
	runtime.KeepAlive(out)
	runtime.KeepAlive(src)
	return out, err
}
