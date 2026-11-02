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
	"time"
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
	// FDFile is what one of them is open on.
	FDFile(pid int, fd int32) (File, error)
	// MaxVnodes is kern.maxvnodes.
	MaxVnodes() (int, error)
}

// File is what a descriptor is open on.
type File struct {
	Path string
	// Regular is whether it is a regular file; Links is how many names
	// it has left (none once deleted), Size its length and Allocated
	// what it takes on disk, in bytes.
	Regular   bool
	Links     int
	Size      int64
	Allocated int64
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
	now func() time.Time
}

// Sample counts what the environment's VM process holds now.
func (f *Finder) Sample() (Sample, error) {
	limit, err := f.Sys.MaxVnodes()
	if err != nil {
		return Sample{}, fmt.Errorf("cannot read kern.maxvnodes: %v", err)
	}
	pid, fds, err := f.locate()
	if err != nil {
		return Sample{}, err
	}
	return Sample{PID: pid, Files: len(fds), Max: limit}, nil
}

// locate is the environment's VM process and its open descriptors of
// files and directories: the one found before while it still is, else
// one found anew.
func (f *Finder) locate() (int, []int32, error) {
	if f.pid != 0 {
		if fds, ok := f.still(); ok {
			return f.pid, fds, nil
		}
		f.pid = 0
	}
	return f.discover()
}

// Deleted is what a VM holds of files deleted on the Mac, at one time.
type Deleted struct {
	PID   int
	Files int // regular files open with no name left
	// Bytes is what they take on disk, about: their allocated blocks,
	// which clones that share blocks count once each.
	Bytes int64
	// Read is how many of the process's Open descriptors were read
	// before the time ran out: all of them, or Files and Bytes are a
	// lower bound.
	Read, Open int
}

// Deleted sums the files the environment's VM process holds open that
// have no name left: deleted on the Mac, they keep their space until the
// process closes them. Files in Exclude (the VM's own disks) are left
// out: what is sought is what the guest has cached from the shares. It
// reads descriptors until by, and says how many it read.
func (f *Finder) Deleted(by time.Time) (Deleted, error) {
	pid, fds, err := f.locate()
	if err != nil {
		return Deleted{}, err
	}
	now := f.now
	if now == nil {
		now = time.Now
	}
	d := Deleted{PID: pid, Open: len(fds)}
	for _, fd := range fds {
		if !now().Before(by) {
			break
		}
		d.Read++
		fi, err := f.Sys.FDFile(pid, fd)
		if err != nil || !fi.Regular || fi.Links > 0 || f.excluded(fi.Path) {
			continue
		}
		d.Files++
		d.Bytes += fi.Allocated
	}
	return d, nil
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
	if fi, err := f.Sys.FDFile(f.pid, f.fd); err != nil || !f.match(fi.Path) {
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
			if fi, err := f.Sys.FDFile(pid, fd); err == nil && f.match(fi.Path) {
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
	return in(p, f.Dirs) && !in(p, f.Exclude)
}

// excluded reports whether p is in one of f.Exclude, under either name
// of the data volume.
func (f *Finder) excluded(p string) bool {
	return in(p, f.Exclude) || strings.HasPrefix(p, dataVolume+"/") && in(strings.TrimPrefix(p, dataVolume), f.Exclude)
}

// in reports whether p is one of dirs or inside one.
func in(p string, dirs []string) bool {
	for _, d := range dirs {
		if within(p, d) {
			return true
		}
	}
	return false
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	dir = filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}
