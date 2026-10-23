//go:build unix && !linux && !darwin

package saferm

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openParent(dir string) (*os.File, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	return os.NewFile(uintptr(fd), dir), nil
}

// chmodUnreadable has no safe way here to change a directory it cannot open.
func chmodUnreadable(dir *os.File, name, shown string, st *unix.Stat_t, mode uint32) (bool, error) {
	return false, fmt.Errorf("saferm: %s: no owner read, and its mode cannot be changed safely on this system", shown)
}

func fixFile(dir *os.File, name, shown string, st *unix.Stat_t) error { return nil }

func clearFlags(f *os.File, shown string, st *unix.Stat_t) error { return nil }
