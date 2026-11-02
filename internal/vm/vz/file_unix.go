//go:build darwin || linux

package vz

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// errLocked is a file another open of it holds the lock of.
var errLocked = errors.New("locked")

// lockFile takes f's exclusive lock (flock) without waiting: errLocked
// when it is held already, through another open of the file.
func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return errLocked
	}
	return err
}
