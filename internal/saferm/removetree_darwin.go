package saferm

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// userFlags are the flags RemoveTree clears: the owner may, and either one
// stops a removal. The SF_ ones need root and are left as they are.
const userFlags = unix.UF_IMMUTABLE | unix.UF_APPEND

// openParent opens the resolved directory holding the tree.
func openParent(dir string) (*os.File, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	return os.NewFile(uintptr(fd), dir), nil
}

// chmodUnreadable sets the mode of the directory name of dir, which st
// says has no owner read, with fchmodat(AT_SYMLINK_NOFOLLOW), which macOS
// honours (on a symlink swapped in, it changes the link itself, which is
// harmless). The open that follows checks it is still that directory.
//
// A directory that is both unreadable and immutable or append-only cannot
// be fixed: its flags must go before its mode can change, and clearing
// them needs a descriptor, which needs read permission (x/sys has no
// lchflags, and a path-based chflags follows a symlink). Such a tree is
// refused with an error saying so, and left as it is.
func chmodUnreadable(dir *os.File, name, shown string, st *unix.Stat_t, mode uint32) (bool, error) {
	if st.Flags&userFlags != 0 {
		return false, fmt.Errorf("saferm: %s: unreadable and immutable or append-only; clear its flags (chflags nouchg,nouappnd) to remove it", shown)
	}
	err := unix.Fchmodat(int(dir.Fd()), name, mode, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(dir)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("saferm: %s: %w", shown, err)
	}
	return true, nil
}

// fixFile clears the user flags of a regular file that has them, through a
// descriptor opened without following and checked to be that file. A file
// that cannot be opened is left for the removal to report.
func fixFile(dir *os.File, name, shown string, st *unix.Stat_t) error {
	if st.Flags&userFlags == 0 {
		return nil
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	runtime.KeepAlive(dir)
	if err != nil {
		return nil
	}
	f := os.NewFile(uintptr(fd), shown)
	defer f.Close()
	var now unix.Stat_t
	err = unix.Fstat(int(f.Fd()), &now)
	runtime.KeepAlive(f)
	if err != nil || !sameObject(st, &now) {
		return nil
	}
	return clearFlags(f, shown, &now)
}

// clearFlags clears the user flags of the object open as f, which st describes.
func clearFlags(f *os.File, shown string, st *unix.Stat_t) error {
	if st.Flags&userFlags == 0 {
		return nil
	}
	err := unix.Fchflags(int(f.Fd()), int(st.Flags&^userFlags))
	runtime.KeepAlive(f)
	if err != nil {
		return fmt.Errorf("saferm: %s: clearing flags: %w", shown, err)
	}
	return nil
}
