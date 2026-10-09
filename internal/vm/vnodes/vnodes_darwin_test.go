package vnodes

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// These run on a Mac only, against the real libproc.

func TestHostCountsItsOwnFiles(t *testing.T) {
	h, err := Host()
	if err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	exe, err := h.Path(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("this process: pid %d, %s", pid, exe)
	pids, err := h.Processes()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pids, pid) {
		t.Fatalf("this user's %d processes leave out this one (%d)", len(pids), pid)
	}
	before, err := h.VnodeFDs(pid)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const n = 5
	var files []*os.File
	for i := range n {
		f, err := os.Create(filepath.Join(dir, "f"+string(rune('0'+i))))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		files = append(files, f)
	}
	after, err := h.VnodeFDs(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("vnode descriptors: %d before, %d after opening %d files", len(before), len(after), n)
	if len(after)-len(before) != n {
		t.Errorf("opening %d files took the count from %d to %d", n, len(before), len(after))
	}
	for _, f := range files {
		fd := int32(f.Fd())
		if !slices.Contains(after, fd) {
			t.Errorf("descriptor %d (%s) is not listed", fd, f.Name())
			continue
		}
		p, err := h.FDPath(pid, fd)
		if err != nil {
			t.Errorf("descriptor %d: %v", fd, err)
		} else if p != f.Name() {
			t.Errorf("descriptor %d: path %q, want %q", fd, p, f.Name())
		}
	}
	limit, err := h.MaxVnodes()
	if err != nil || limit <= 0 {
		t.Fatalf("kern.maxvnodes: %d, %v", limit, err)
	}
	t.Logf("kern.maxvnodes: %d", limit)
}

func TestHostListsVMs(t *testing.T) {
	h, err := Host()
	if err != nil {
		t.Fatal(err)
	}
	limit, err := h.MaxVnodes()
	if err != nil {
		t.Fatal(err)
	}
	pids, err := h.Processes()
	if err != nil {
		t.Fatal(err)
	}
	vms := 0
	for _, pid := range pids {
		if p, err := h.Path(pid); err != nil || filepath.Base(p) != ProcessName {
			continue
		}
		vms++
		fds, err := h.VnodeFDs(pid)
		if err != nil {
			t.Errorf("VM process %d: %v", pid, err)
			continue
		}
		t.Logf("VM process %d holds %d files and directories, %.1f%% of kern.maxvnodes %d",
			pid, len(fds), 100*float64(len(fds))/float64(limit), limit)
		for _, fd := range fds[:min(len(fds), 25)] {
			p, err := h.FDPath(pid, fd)
			if err != nil {
				t.Logf("  fd %d: %v", fd, err)
			} else {
				t.Logf("  fd %d: %s", fd, p)
			}
		}
	}
	if vms == 0 {
		t.Skip("no VM is running")
	}
	// Each caboose environment's data dir, as a Finder would match it.
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	envs, _ := filepath.Glob(filepath.Join(home, ".caboose", "envs", "*", "data"))
	for _, d := range envs {
		f := &Finder{Sys: h, Dirs: []string{d}, Exclude: []string{filepath.Join(d, "vm")}}
		s, err := f.Sample()
		if err != nil {
			t.Logf("environment %s: %v", filepath.Base(filepath.Dir(d)), err)
		} else {
			t.Logf("environment %s: VM process %d holds %d of %d", filepath.Base(filepath.Dir(d)), s.PID, s.Files, s.Max)
		}
	}
}
