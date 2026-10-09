// Package vnodes counts the files a VM holds open on the Mac, against the
// Mac's limit on vnodes (kern.maxvnodes).
//
// Virtualization.framework serves a VM's virtio-fs shares from a process
// of its own, com.apple.Virtualization.VirtualMachine, launched for each
// VM by launchd rather than by caboose-vmm, which owns the VM. That
// process keeps a file descriptor open for every file and directory the
// guest has cached from the shares, and a busy guest caches enough of
// them to take most of the Mac's vnodes, and then every program on the
// Mac fails to open a file (ENFILE).
//
// Nothing public maps a VM to its process, so a Finder tells the VM of
// an environment by what it holds: the process among this user's
// Virtualization processes with a file open under the environment's own
// directories, which no other environment shares (roots can be). On a
// Mac the processes are read through libproc, by purego (the launcher is
// built without cgo); elsewhere Host says ErrUnsupported.
package vnodes

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// ProcessName is the executable's name of the process that runs a VM of
// Virtualization.framework's.
const ProcessName = "com.apple.Virtualization.VirtualMachine"

// maxProbes bounds the paths a discovery reads of one process: the shares
// are opened at boot, so their files are among its lowest descriptors.
const maxProbes = 4096

// ErrUnsupported is a platform with no such processes to read.
var ErrUnsupported = errors.New("counting a VM's open files is supported on macOS only, not " + runtime.GOOS)

// System is what a Finder reads of the machine; Host is the real one.
type System interface {
	// Processes are the PIDs of this user's processes.
	Processes() ([]int, error)
	// Path is a process's executable.
	Path(pid int) (string, error)
	// VnodeFDs are a process's open descriptors of files and directories,
	// in ascending order.
	VnodeFDs(pid int) ([]int32, error)
	// FDPath is the path of one of them.
	FDPath(pid int, fd int32) (string, error)
	// MaxVnodes is kern.maxvnodes.
	MaxVnodes() (int, error)
}

// Sample is what a VM holds, at one time.
type Sample struct {
	PID   int // its Virtualization process
	Files int // the files and directories that process holds open
	Max   int // kern.maxvnodes
}

// Finder finds an environment's VM process and samples it. It keeps the
// process it found, and finds it again when that one has gone.
type Finder struct {
	Sys System
	// Dirs are the environment's own directories: a process with a file
	// open in one, but in none of Exclude, is its VM's.
	Dirs, Exclude []string

	pid int
	fd  int32 // the descriptor that showed pid is the environment's
}

// Sample counts what the environment's VM process holds now.
func (f *Finder) Sample() (Sample, error) {
	limit, err := f.Sys.MaxVnodes()
	if err != nil {
		return Sample{}, fmt.Errorf("cannot read kern.maxvnodes: %v", err)
	}
	if f.pid != 0 {
		if fds, ok := f.still(); ok {
			return Sample{PID: f.pid, Files: len(fds), Max: limit}, nil
		}
		f.pid = 0
	}
	pid, fds, err := f.discover()
	if err != nil {
		return Sample{}, err
	}
	return Sample{PID: pid, Files: len(fds), Max: limit}, nil
}

// still reports whether the process found before is still the
// environment's VM: the same executable, and the descriptor that showed
// it still open on a file of the environment's. A PID reused since fails
// one or the other.
func (f *Finder) still() ([]int32, bool) {
	if p, err := f.Sys.Path(f.pid); err != nil || filepath.Base(p) != ProcessName {
		return nil, false
	}
	fds, err := f.Sys.VnodeFDs(f.pid)
	if err != nil {
		return nil, false
	}
	if p, err := f.Sys.FDPath(f.pid, f.fd); err != nil || !f.match(p) {
		return nil, false
	}
	return fds, true
}

// discover looks through this user's Virtualization processes for the
// environment's.
func (f *Finder) discover() (int, []int32, error) {
	pids, err := f.Sys.Processes()
	if err != nil {
		return 0, nil, fmt.Errorf("cannot list processes: %v", err)
	}
	vms := 0
	for _, pid := range pids {
		if p, err := f.Sys.Path(pid); err != nil || filepath.Base(p) != ProcessName {
			continue
		}
		vms++
		fds, err := f.Sys.VnodeFDs(pid)
		if err != nil {
			continue
		}
		for _, fd := range fds[:min(len(fds), maxProbes)] {
			if p, err := f.Sys.FDPath(pid, fd); err == nil && f.match(p) {
				f.pid, f.fd = pid, fd
				return pid, fds, nil
			}
		}
	}
	if vms == 0 {
		return 0, nil, errors.New("no VM is running")
	}
	return 0, nil, fmt.Errorf("none of the %d running VMs holds a file of this environment's", vms)
}

// dataVolume is where a Mac's firmlinks point: the kernel may give a
// path under /Users through it.
const dataVolume = "/System/Volumes/Data"

// match reports whether p is in one of f.Dirs and none of f.Exclude.
func (f *Finder) match(p string) bool {
	if strings.HasPrefix(p, dataVolume+"/") {
		if f.match(strings.TrimPrefix(p, dataVolume)) {
			return true
		}
	}
	in := func(dirs []string) bool {
		for _, d := range dirs {
			if within(p, d) {
				return true
			}
		}
		return false
	}
	return in(f.Dirs) && !in(f.Exclude)
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	dir = filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}
