package vm

import (
	"errors"

	"golang.org/x/sys/unix"
)

func clonefile(src, dst string) error {
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) {
		return errNoClone
	}
	return err
}
