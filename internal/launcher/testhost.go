package launcher

import (
	"errors"
	"io/fs"

	"github.com/bfreis/caboose/internal/vm/vnodes"
)

// PlayHostForTests has the launcher see a host of its own instead of the
// machine running it, for the tests of the packages that drive it
// (cmd/caboose), whose output must not depend on that machine: the OS
// goos, one checkpoint volume (ext4, or APFS on darwin) with room, no ssh and no ~/.ssh to find
// the default SSH agent with, kern.maxvnodes at what vm wants on darwin
// (none elsewhere), and no processes to count a VM's open files in. It returns what undoes it. These are package
// variables: a test that calls it must not run in parallel with another
// that runs the launcher. Only tests call it.
func PlayHostForTests(goos_ string) (restore func()) {
	oldGOOS, oldCk, oldSSH := goos, hostCkFS, hostSSH
	oldMax, oldVnodes := maxVnodesNow, vnodesHost
	fstype := "ext4"
	if goos_ == "darwin" {
		fstype = "apfs"
	}
	goos = goos_
	hostCkFS = ckFS{goos: goos_,
		info:   func(string) (uint64, string, error) { return 1, fstype, nil },
		statfs: func(string) (uint64, uint64, error) { return 400 << 30, 1000 << 30, nil }}
	hostSSH = &SSHLookup{
		G:    func(string) ([]byte, error) { return nil, errors.New("no ssh on PATH") },
		Home: func() (string, error) { return "/nonexistent-home", nil },
		FS:   noFS{},
	}
	maxVnodesNow = func() (uint32, error) { return 0, errNoVnodes }
	if goos_ == "darwin" {
		maxVnodesNow = func() (uint32, error) { return wantMaxVnodes, nil }
	}
	vnodesHost = func() (vnodes.System, error) { return nil, vnodes.ErrUnsupported }
	return func() {
		goos, hostCkFS, hostSSH = oldGOOS, oldCk, oldSSH
		maxVnodesNow, vnodesHost = oldMax, oldVnodes
	}
}

// noFS is a file system with no files.
type noFS struct{}

func (noFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}
