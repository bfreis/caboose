package vz

import (
	"os"

	"golang.org/x/sys/unix"
)

// fullSync asks for f's data on stable storage, not only on the drive:
// fsync on a Mac stops at the drive's cache.
func fullSync(f *os.File) error {
	_, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0)
	return err
}

// punchHole gives n bytes at off back to the file system, both multiples
// of the file system's block: they read as zeroes after.
func punchHole(f *os.File, off, n int64) error {
	// struct fpunchhole (flags, reserved, offset, length) is the start of
	// fstore_t, field for field.
	p := unix.Fstore_t{Offset: off, Length: n}
	return unix.FcntlFstore(f.Fd(), unix.F_PUNCHHOLE, &p)
}

// fsBlock is the block of f's file system, what a hole is made of.
func fsBlock(f *os.File) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	return int64(st.Bsize), nil
}

// fNoCacheExt is F_NOCACHE_EXT of <sys/fcntl.h>: F_NOCACHE, and a write
// aligned to the file system's block, in offset and length, goes to the
// disk directly even when that block is smaller than a page.
const fNoCacheExt = 112

// noCache has f's reads and writes skip the Mac's page cache, as the
// framework's own disks do: what the guest reads it caches itself.
func noCache(f *os.File) error {
	_, err := unix.FcntlInt(f.Fd(), fNoCacheExt, 1)
	return err
}
