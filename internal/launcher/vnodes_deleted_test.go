package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/vm/vnodes"
)

// deletedSys is a Mac with one VM process, pid 7, holding files: by
// descriptor, from 3 up, each regular with one link but for those in gone,
// which have none and take the bytes given.
type deletedSys struct {
	files []string
	gone  map[int]int64
}

func (s *deletedSys) Processes() ([]int, error) { return []int{7}, nil }
func (s *deletedSys) Path(pid int) (string, error) {
	return "/System/Library/Frameworks/Virtualization.framework/XPCServices/" + vnodes.ProcessName, nil
}
func (s *deletedSys) VnodeFDs(pid int) ([]int32, error) {
	var fds []int32
	for i := range s.files {
		fds = append(fds, int32(3+i))
	}
	return fds, nil
}
func (s *deletedSys) FDFile(pid int, fd int32) (vnodes.File, error) {
	i := int(fd) - 3
	if i < 0 || i >= len(s.files) {
		return vnodes.File{}, errors.New("not open")
	}
	if n, ok := s.gone[i]; ok {
		return vnodes.File{Path: s.files[i], Regular: true, Size: n, Allocated: n}, nil
	}
	return vnodes.File{Path: s.files[i], Regular: true, Links: 1}, nil
}
func (s *deletedSys) MaxVnodes() (int, error) { return 1000, nil }

// vmFilesRows runs doctorVMFiles for an environment whose data dir is
// data, on a Mac playing sys, and gives each row as "label: text | fix".
func vmFilesRows(t *testing.T, sys vnodes.System, data string) []string {
	t.Helper()
	return appVMFilesRows(t, sys, &App{Cfg: &config.Config{DataDir: data}})
}

// appVMFilesRows is vmFilesRows for a.
func appVMFilesRows(t *testing.T, sys vnodes.System, a *App) []string {
	t.Helper()
	old := vnodesHost
	t.Cleanup(func() { vnodesHost = old })
	vnodesHost = func() (vnodes.System, error) { return sys, nil }
	c := &checkup{}
	a.doctorVMFiles(c)
	var rows []string
	for _, r := range c.rows {
		rows = append(rows, r.label+": "+r.text+" | "+r.fix)
	}
	return rows
}

func TestDoctorVMDeletedFiles(t *testing.T) {
	for _, c := range []struct {
		name string
		gone map[int]int64
		want string
	}{
		{"none", nil, "VM's deleted files: the VM holds no files deleted on the Mac open | "},
		{"a few", map[int]int64{1: 300 << 10, 2: 4 << 10}, "VM's deleted files: the VM holds 2 files deleted on the Mac open, 0.3 MiB of disk | "},
		{"large", map[int]int64{1: 1 << 30, 2: 512 << 20},
			"VM's deleted files: the VM holds 2 files deleted on the Mac open, 1.5 GiB of disk: the space comes back only when the guest lets go of them | in the sandbox, sync; echo 2 > /proc/sys/vm/drop_caches"},
		// The VM's own disks are not what the guest cached.
		{"its disks", map[int]int64{3: 8 << 30}, "VM's deleted files: the VM holds no files deleted on the Mac open | "},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The finder matches the data dir by its real path, as the
			// kernel names it; t.TempDir may be under a link (a Mac's /var).
			data := t.TempDir()
			real, err := filepath.EvalSymlinks(data)
			if err != nil {
				t.Fatal(err)
			}
			sys := &deletedSys{gone: c.gone, files: []string{real + "/home/.claude.json", "/Users/u/dev/big.bin", "/Users/u/dev/small.bin", real + "/vm/images/root-old.img"}}
			rows := vmFilesRows(t, sys, data)
			if len(rows) != 2 || !strings.HasPrefix(rows[0], "VM's open files: the VM holds 4 of the Mac's 1,000 vnodes open") {
				t.Fatalf("rows %q, want the open files' then the deleted files'", rows)
			}
			if !strings.HasPrefix(rows[1], c.want) {
				t.Errorf("got %q\nwant %q...", rows[1], c.want)
			}
		})
	}
}

func TestDoctorVMDeletedPartial(t *testing.T) {
	for _, c := range []struct {
		d    vnodes.Deleted
		want string
	}{
		{vnodes.Deleted{Read: 10, Open: 20}, "the VM holds no file deleted on the Mac open among the 10 of its 20 open files read in 3s"},
		{vnodes.Deleted{Files: 1, Bytes: 2 << 30, Read: 1500, Open: 20000},
			"the VM holds at least 1 file deleted on the Mac open, 2.0 GiB of disk (1,500 of its 20,000 open files read in 3s): the space comes back only when the guest lets go of them"},
		{vnodes.Deleted{Files: 1, Bytes: vmDeletedLarge - 1, Read: 5, Open: 5},
			"the VM holds 1 file deleted on the Mac open, 1024.0 MiB of disk"},
		{vnodes.Deleted{Files: 3, Bytes: vmDeletedLarge, Read: 5, Open: 5},
			"the VM holds 3 files deleted on the Mac open, 1.0 GiB of disk: the space comes back only when the guest lets go of them"},
	} {
		ck := &checkup{}
		doctorVMDeleted(ck, c.d)
		if len(ck.rows) != 1 || ck.rows[0].text != c.want {
			t.Errorf("%+v: rows %+v, want %q", c.d, ck.rows, c.want)
		}
		if large := c.d.Bytes >= vmDeletedLarge; (ck.rows[0].level == levelProblem) != large {
			t.Errorf("%+v: level %v, want a problem only from %d bytes", c.d, ck.rows[0].level, vmDeletedLarge)
		}
	}
}

func TestDoctorVMFilesNoVM(t *testing.T) {
	sys := &deletedSys{files: []string{"/Users/u/elsewhere"}}
	if rows := vmFilesRows(t, sys, t.TempDir()); len(rows) != 0 {
		t.Fatalf("a VM of another environment's: rows %q, want none", rows)
	}
}

// The VM's process keeps its kernel open, and an update deletes an old
// version's: no file the guest cached. Nor is a checkout's, rebuilt.
func TestDoctorVMDeletedLeavesTheKernelOut(t *testing.T) {
	real := func(d string) string {
		r, err := filepath.EvalSymlinks(d)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	data, home, checkout := real(t.TempDir()), real(t.TempDir()), real(t.TempDir())
	sys := &deletedSys{
		files: []string{
			data + "/home/.claude.json",
			home + "/vm/v0.4.1/arm64/kernel-arm64",
			checkout + "/vm-dist/kernel-" + runtime.GOARCH,
			checkout + "/vm-dist/e2e/held.bin",
		},
		gone: map[int]int64{1: 11 << 20, 2: 11 << 20, 3: 1 << 20},
	}
	rows := appVMFilesRows(t, sys, &App{Cfg: &config.Config{DataDir: data, CabooseHome: home}, Checkout: checkout})
	want := "VM's deleted files: the VM holds 1 file deleted on the Mac open, 1.0 MiB of disk | "
	if len(rows) != 2 || rows[1] != want {
		t.Fatalf("rows %q, want the second %q", rows, want)
	}
}

// The environment may be given by paths through symlinks, while the
// kernel names the files the VM holds by their real ones: the exclusions
// are matched under both spellings.
func TestDoctorVMDeletedLeavesTheKernelOutBehindSymlinks(t *testing.T) {
	real := func(d string) string {
		r, err := filepath.EvalSymlinks(d)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	link := func(target string) string {
		l := filepath.Join(t.TempDir(), "l")
		if err := os.Symlink(target, l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	data, home, checkout := real(t.TempDir()), real(t.TempDir()), real(t.TempDir())
	// The excluded paths exist, or there is no real path to find.
	for _, d := range []string{home + "/vm", checkout + "/vm-dist"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	kernel := checkout + "/vm-dist/kernel-" + runtime.GOARCH
	if err := os.WriteFile(kernel, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sys := &deletedSys{
		files: []string{
			data + "/home/.claude.json",
			home + "/vm/v0.4.1/arm64/kernel-arm64",
			kernel,
		},
		gone: map[int]int64{1: 11 << 20, 2: 11 << 20},
	}
	a := &App{Cfg: &config.Config{DataDir: link(data), CabooseHome: link(home)}, Checkout: link(checkout)}
	rows := appVMFilesRows(t, sys, a)
	want := "VM's deleted files: the VM holds no files deleted on the Mac open | "
	if len(rows) != 2 || rows[1] != want {
		t.Fatalf("rows %q, want the second %q: the kernels left out", rows, want)
	}
}
