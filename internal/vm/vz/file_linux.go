package vz

import (
	"os"

	"golang.org/x/sys/unix"
)

// fullSync is fsync off a Mac.
func fullSync(f *os.File) error { return f.Sync() }

// punchHole gives n bytes at off back to the file system: they read as
// zeroes after.
func punchHole(f *os.File, off, n int64) error {
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, n)
}

// fsBlock is the block of f's file system, what a hole is made of.
func fsBlock(f *os.File) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	return st.Bsize, nil
}
