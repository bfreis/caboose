//go:build !darwin && !linux

package vz

import (
	"errors"
	"os"
)

// fullSync is fsync.
func fullSync(f *os.File) error { return f.Sync() }

// punchHole writes n zero bytes at off, with no hole to make.
func punchHole(f *os.File, off, n int64) error {
	d := blkDisk{f: f}
	return d.writeZeroes(off, n)
}

// fsBlock is a block of 4 KiB, with no file system to ask.
func fsBlock(*os.File) (int64, error) { return 4096, nil }

// lockFile takes nothing, with no lock to take.
func lockFile(*os.File) error { return nil }

var errLocked = errors.New("locked")
