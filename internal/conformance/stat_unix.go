//go:build unix

package conformance

import (
	"io/fs"
	"os"
	"syscall"
)

// currentUID is the user the suite runs as; a variable for tests only.
var currentUID = os.Getuid

func ownerOf(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// sysIdentity is the device, inode and link count of fi.
func sysIdentity(fi fs.FileInfo) (dev, ino, nlink uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), uint64(st.Nlink), true
}
