//go:build unix && !linux && !darwin

package checkpoint

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// chmodUnopenable has no safe way here to change an entry it cannot open.
func chmodUnopenable(d *os.File, name string, st *unix.Stat_t, mode uint32) (bool, error) {
	return false, errors.New("its owner cannot open it, and its mode cannot be changed safely on this system")
}
