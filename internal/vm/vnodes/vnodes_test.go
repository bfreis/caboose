package vnodes

import (
	"errors"
	"strings"
	"testing"
)

const vmPath = "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/" + ProcessName + ".xpc/Contents/MacOS/" + ProcessName

// fakeProc is a process of fakeSys: its executable and its open files, by
// descriptor.
type fakeProc struct {
	path  string
	files map[int32]string
}

type fakeSys struct {
	procs  map[int]*fakeProc
	max    int
	probes int // FDPath calls
}

func (s *fakeSys) Processes() ([]int, error) {
	var pids []int
	for pid := 1; pid < 100; pid++ {
		if s.procs[pid] != nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (s *fakeSys) Path(pid int) (string, error) {
	if p := s.procs[pid]; p != nil {
		return p.path, nil
	}
	return "", errors.New("no such process")
}

func (s *fakeSys) VnodeFDs(pid int) ([]int32, error) {
	p := s.procs[pid]
	if p == nil {
		return nil, errors.New("no such process")
	}
	var fds []int32
	for fd := int32(0); fd < 10000; fd++ {
		if _, ok := p.files[fd]; ok {
			fds = append(fds, fd)
		}
	}
	return fds, nil
}

func (s *fakeSys) FDPath(pid int, fd int32) (string, error) {
	s.probes++
	p := s.procs[pid]
	if p == nil {
		return "", errors.New("no such process")
	}
	f, ok := p.files[fd]
	if !ok {
		return "", errors.New("not open")
	}
	return f, nil
}

func (s *fakeSys) MaxVnodes() (int, error) { return s.max, nil }

// vmOf is a VM process holding files, from descriptor 3 up.
func vmOf(files ...string) *fakeProc {
	p := &fakeProc{path: vmPath, files: map[int32]string{}}
	for i, f := range files {
		p.files[int32(3+i)] = f
	}
	return p
}

const (
	dataDir  = "/Users/u/.caboose/envs/default/data"
	otherDir = "/Users/u/.caboose/envs/other/data"
)

func finder(s *fakeSys) *Finder {
	return &Finder{Sys: s, Dirs: []string{dataDir}, Exclude: []string{dataDir + "/vm"}}
}

func TestSampleFindsTheEnvironmentsVM(t *testing.T) {
	s := &fakeSys{max: 1000, procs: map[int]*fakeProc{
		// Another environment's VM, sharing a root with this one's.
		5: vmOf("/Users/u/dev/a.go", "/Users/u/dev/b.go", otherDir+"/home/.claude"),
		// Not a VM, though it holds this environment's files.
		6: {path: "/usr/bin/vim", files: map[int32]string{3: dataDir + "/home/.claude/x"}},
		// This environment's, its own files after the shared ones.
		7: vmOf("/Users/u/dev/a.go", dataDir+"/home/.claude", "/Users/u/dev/c.go", "/Users/u/dev/d.go"),
	}}
	got, err := finder(s).Sample()
	if err != nil {
		t.Fatal(err)
	}
	if got != (Sample{PID: 7, Files: 4, Max: 1000}) {
		t.Fatalf("got %+v, want pid 7 holding 4 of 1000", got)
	}
}

func TestSampleIgnoresExcludedAndLookalikePaths(t *testing.T) {
	s := &fakeSys{max: 1000, procs: map[int]*fakeProc{
		// The builder's disks are under the data dir's vm/.
		4: vmOf(dataDir + "/vm/builder/disk.img"),
		// A sibling whose name starts with the data dir's.
		5: vmOf(dataDir + "2/home/x"),
	}}
	if got, err := finder(s).Sample(); err == nil {
		t.Fatalf("got %+v, want no VM found", got)
	} else if !strings.Contains(err.Error(), "none of the 2 running VMs") {
		t.Fatalf("got %v", err)
	}
}

func TestSampleMatchesThroughTheDataVolume(t *testing.T) {
	s := &fakeSys{max: 1000, procs: map[int]*fakeProc{
		3: vmOf(dataVolume + dataDir + "/home"),
	}}
	if got, err := finder(s).Sample(); err != nil || got.PID != 3 {
		t.Fatalf("got %+v, %v; want pid 3", got, err)
	}
}

func TestSampleNoVM(t *testing.T) {
	s := &fakeSys{max: 1000, procs: map[int]*fakeProc{6: {path: "/usr/bin/vim"}}}
	if _, err := finder(s).Sample(); err == nil || err.Error() != "no VM is running" {
		t.Fatalf("got %v, want no VM is running", err)
	}
}

func TestSampleKeepsTheProcessItFound(t *testing.T) {
	s := &fakeSys{max: 1000, procs: map[int]*fakeProc{
		5: vmOf(otherDir + "/home"),
		7: vmOf(dataDir + "/home"),
	}}
	f := finder(s)
	if _, err := f.Sample(); err != nil {
		t.Fatal(err)
	}
	s.procs[7].files[20] = "/Users/u/dev/new.go"
	s.probes = 0
	got, err := f.Sample()
	if err != nil || got != (Sample{PID: 7, Files: 2, Max: 1000}) {
		t.Fatalf("got %+v, %v; want pid 7 holding 2", got, err)
	}
	if s.probes != 1 {
		t.Fatalf("a known process took %d path reads, want 1", s.probes)
	}
}

func TestSampleFindsAgainWhenTheProcessGoes(t *testing.T) {
	s := &fakeSys{max: 1000, procs: map[int]*fakeProc{
		7: vmOf(dataDir + "/home"),
	}}
	f := finder(s)
	if _, err := f.Sample(); err != nil {
		t.Fatal(err)
	}
	// The VM stopped: nothing to find.
	delete(s.procs, 7)
	if _, err := f.Sample(); err == nil {
		t.Fatal("found a VM that is gone")
	}
	// Its PID now another environment's VM, and this one's back as 9.
	s.procs[7] = vmOf(otherDir + "/home")
	s.procs[9] = vmOf("/Users/u/dev/a.go", dataDir+"/home")
	got, err := f.Sample()
	if err != nil || got.PID != 9 || got.Files != 2 {
		t.Fatalf("got %+v, %v; want pid 9 holding 2", got, err)
	}
	// Its PID reused by another VM while it was known: found anew.
	s.procs[9] = vmOf(otherDir + "/home")
	s.procs[11] = vmOf(dataDir + "/home")
	if got, err := f.Sample(); err != nil || got.PID != 11 {
		t.Fatalf("got %+v, %v; want pid 11", got, err)
	}
}

func TestSampleBoundsTheProbes(t *testing.T) {
	p := &fakeProc{path: vmPath, files: map[int32]string{}}
	for fd := int32(0); fd < maxProbes+10; fd++ {
		p.files[fd] = "/Users/u/dev/x"
	}
	p.files[maxProbes+5] = dataDir + "/home"
	s := &fakeSys{max: 1000, procs: map[int]*fakeProc{3: p}}
	if _, err := finder(s).Sample(); err == nil {
		t.Fatal("found a VM by a file past the probes")
	}
	if s.probes != maxProbes {
		t.Fatalf("read %d paths, want %d", s.probes, maxProbes)
	}
}
