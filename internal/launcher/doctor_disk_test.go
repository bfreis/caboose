package launcher

import (
	"net"
	"os"
	"strings"
	"testing"
)

// doctor's disk row sums the VM disks from their stat alone, and names
// each leftover past its threshold with what frees it: a large builder
// cache, the scratch disk of a VM not running, root disks nothing uses,
// a volume's disk no profile declares. Status has the total.
func TestDoctorDiskRow(t *testing.T) {
	e := diskEnv(t)
	alloc := diskAllocated
	diskAllocated = func(fi os.FileInfo) int64 { return fi.Size() / 4 }
	t.Cleanup(func() { diskAllocated = alloc })
	c := &checkup{}
	e.a.doctorDisk(c)
	var got []string
	for _, r := range c.rows {
		if r.label != "disk" {
			t.Errorf("label %q", r.label)
		}
		got = append(got, r.text)
	}
	tmp := e.a.Cfg.DataDir
	want := []string{
		"VM disks: 139 GB allocated (root disks 34 GB, empty disks 1.1 GB, scratch disks 17 GB, volumes 52 GB, builder cache 34 GB); 'caboose disk' shows the rest, checkpoints included, and what deleting each would free",
		"the builder's cache, " + tmp + "/vm/builder/cache.img, holds 34 GB and only grows: 'caboose prune --builder-cache' deletes it (asking first), and the next build makes it again, fetching everything anew",
		tmp + "/vm/caboose-default/scratch.img, the scratch disk of a VM not running, holds 17 GB: 'caboose prune' deletes it (a start makes a new one)",
		"1 root disk no image or VM uses holds 17 GB: the next 'caboose build' deletes it",
		"volume old's disk holds 8.6 GB and no profile declares it: 'caboose volume rm old --delete-disk' deletes it",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if l := e.a.diskStatusLine(); l != "VM disks 139 GB allocated ('caboose disk' shows everything)" {
		t.Errorf("status: %q", l)
	}

	// Below the thresholds, the row alone.
	diskAllocated = func(fi os.FileInfo) int64 { return fi.Size() / 256 }
	c = &checkup{}
	e.a.doctorDisk(c)
	if len(c.rows) != 1 {
		t.Errorf("below the thresholds: %+v", c.rows)
	}
}

// The scratch disk of a VM that runs is no leftover.
func TestDoctorDiskRunningScratch(t *testing.T) {
	e := diskEnv(t)
	alloc := diskAllocated
	diskAllocated = func(fi os.FileInfo) int64 { return fi.Size() / 4 }
	t.Cleanup(func() { diskAllocated = alloc })
	sock := e.a.vmDir(e.a.Cfg.Container).Socket()
	if len(sock) >= maxSocketPath {
		t.Skipf("socket path too long here: %s", sock)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	c := &checkup{}
	e.a.doctorDisk(c)
	for _, r := range c.rows {
		if strings.Contains(r.text, "scratch disk of a VM not running") {
			t.Errorf("running VM's scratch named: %s", r.text)
		}
	}
}

// No vm/ in the data dir, no row and no status line.
func TestDoctorDiskNone(t *testing.T) {
	e, _ := adminEnv(t, adminConfig, nil)
	c := &checkup{}
	e.a.doctorDisk(c)
	if len(c.rows) != 0 || e.a.diskStatusLine() != "" {
		t.Errorf("%+v %q", c.rows, e.a.diskStatusLine())
	}
}

// Each threshold notes from its own size up, and not a byte below: a
// builder cache of 16 GiB, a leftover of 1 GiB.
func TestDoctorDiskThresholds(t *testing.T) {
	e := diskEnv(t)
	alloc := diskAllocated
	t.Cleanup(func() { diskAllocated = alloc })
	for _, off := range []int64{0, -1} {
		diskAllocated = func(fi os.FileInfo) int64 {
			switch {
			case fi.Name() == "cache.img":
				return builderCacheNote + off
			case fi.Name() == "scratch.img", strings.HasPrefix(fi.Name(), "root-"):
				return leftoverNote + off
			}
			return 0
		}
		// The volumes' disks hold nothing here: no orphan note in either
		// run. One unused root disk of the two.
		c := &checkup{}
		e.a.doctorDisk(c)
		var builder, scratch, roots bool
		for _, r := range c.rows {
			builder = builder || strings.Contains(r.text, "the builder's cache")
			scratch = scratch || strings.Contains(r.text, "the scratch disk of a VM not running")
			roots = roots || strings.Contains(r.text, "root disk no image or VM uses")
		}
		if want := off == 0; builder != want || scratch != want || roots != want {
			t.Errorf("%+d: builder %v scratch %v roots %v", off, builder, scratch, roots)
		}
	}
}
