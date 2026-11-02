package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/vm"
)

// TestVolumes boots a VM with dockerd's disk and two volumes on
// caboose-vmm's device, of sizes of their own, as the launcher creates a
// sandbox: each is found by its serial and mounted at its path with
// discard, its file system grown to fill it. Then the VM stops and starts
// again with one size raised and the other lowered: the raised one's
// disk and file system grow, the lowered one's are kept as they were
// (a start never shrinks a disk), and what was written survives on both.
func TestVolumes(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" {
		t.Skip("CABOOSE_VM_E2E names no directory of VM files")
	}
	if fi, err := os.Stat(host{dir: dir}.emptyDisk()); err != nil || fi.Size() != config.EmptyDiskSize {
		t.Fatalf("the empty disk must be the %dG one the builder makes: %v", config.EmptyDiskSize>>30, err)
	}
	work, err := os.MkdirTemp("/tmp", "cvv")
	if err != nil {
		t.Fatal(err)
	}
	h := host{dir: dir, work: work, volSize: 4 << 30, sizes: map[string]int64{"alpha": 6 << 30, "beta": 8 << 30}}
	vmdir := vm.Dir(filepath.Join(work, "vm"))
	v := backend.NewVM("e2e-volumes", vmdir, h)
	v.Self, _ = os.Executable()
	v.Ready = ready
	defer func() {
		for _, f := range []string{vmdir.Log(), vmdir.Console()} {
			if b, err := os.ReadFile(f); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "volumes-"+filepath.Base(f)), b, 0o644)
			}
		}
		_ = v.Remove()
		os.RemoveAll(work)
	}()
	start := time.Now()
	out := func(script string) string {
		t.Helper()
		s, err := backend.Output(v, "sh", "-c", script+" 2>&1")
		if err != nil {
			t.Errorf("%s: %v\n%s", script, err, s)
		}
		return s
	}
	// check is what the guest has of each volume: its mount, the size of
	// its disk and of its file system.
	check := func(when string, sizes map[string]int64) {
		t.Helper()
		mounts := out(`cat /proc/mounts`)
		for name, target := range map[string]string{"docker": "/var/lib/docker", "alpha": "/vol/alpha", "beta": "/root/.cache/beta"} {
			var line string
			for _, l := range strings.Split(mounts, "\n") {
				if f := strings.Fields(l); len(f) > 3 && f[1] == target {
					line = l
				}
			}
			if !strings.Contains(line, " ext4 ") || !strings.Contains(line, "discard") {
				t.Errorf("%s: %s at %s: %q", when, name, target, line)
			}
			size, ok := sizes[name]
			if !ok {
				continue
			}
			dev := strings.Fields(line)[0]
			sectors, _ := strconv.ParseInt(out(`cat /sys/block/`+strings.TrimPrefix(dev, "/dev/")+`/size`), 10, 64)
			kib, _ := strconv.ParseInt(out(`df -k `+target+` | tail -1 | awk '{print $2}'`), 10, 64)
			step(t, start, "%s: %s is %s (serial %q), %d sectors, its file system %d KiB; the disk on the Mac %d bytes", when, name, dev,
				out(`cat /sys/block/`+strings.TrimPrefix(dev, "/dev/")+`/serial`), sectors, kib, fileSize(t, filepath.Join(work, "volumes", name+".img")))
			if sectors*512 != size {
				t.Errorf("%s: %s's disk is %d bytes in the guest, want %d", when, name, sectors*512, size)
			}
			if fileSize(t, filepath.Join(work, "volumes", name+".img")) != size {
				t.Errorf("%s: %s's disk on the Mac is not %d bytes", when, name, size)
			}
			// The file system fills the disk, less its own overhead.
			if kib*1024 < size*9/10 || kib*1024 > size {
				t.Errorf("%s: %s's file system is %d KiB on a disk of %d bytes", when, name, kib, size)
			}
		}
	}

	if err := v.Create(backend.Spec{
		Image: "e2e", Hostname: "caboose-e2e", User: "0:0",
		Volumes: []backend.Volume{
			{Name: "alpha", Target: "/vol/alpha", Live: liveVolumes()},
			{Name: "docker", Target: "/var/lib/docker"},
			{Name: "beta", Target: "/root/.cache/beta", Live: liveVolumes()},
		},
		Labels: []string{"caboose.isolation=vm"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
	step(t, start, "ready; disks: %s", out(`for d in /sys/block/vd*; do echo -n "${d##*/}=$(cat $d/serial 2>/dev/null) "; done`))
	check("first boot", map[string]int64{"alpha": 6 << 30, "beta": 8 << 30, "docker": 4 << 30})
	sums := out(`for d in /vol/alpha /root/.cache/beta; do dd if=/dev/urandom of=$d/f bs=1M count=64 2>/dev/null; sha256sum < $d/f; done; sync`)
	step(t, start, "written: %s", strings.ReplaceAll(sums, "\n", " "))

	if err := v.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// alpha raised, beta lowered in config.toml.
	h.sizes["alpha"], h.sizes["beta"] = 10<<30, 4<<30
	if err := v.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready again: %v", err)
	}
	step(t, start, "started again")
	check("second boot", map[string]int64{"alpha": 10 << 30, "beta": 8 << 30, "docker": 4 << 30})
	again := out(`echo 3 > /proc/sys/vm/drop_caches; for d in /vol/alpha /root/.cache/beta; do sha256sum < $d/f; done`)
	if again != sums || sums == "" {
		t.Errorf("the volumes' data: %q, written %q", again, sums)
	}
	step(t, start, "errors in dmesg: %s", out(`dmesg | grep -i -E 'error|fail|EXT4-fs (warning|error)' | tail -5; true`))
	if err := v.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	log, _ := os.ReadFile(vmdir.Log())
	for _, l := range strings.Split(string(log), "\n") {
		if strings.Contains(l, "blk") {
			t.Logf("vmm.log: %s", l)
		}
	}
}

// TestVolumesCount boots a VM as the launcher makes a sandbox -- a share,
// dockerd's disk -- with config.MaxVMVolumes volumes on caboose-vmm's
// device (CABOOSE_VM_E2E_VOLUMES tries another number), and checks the
// guest mounts every one (on the framework's own disks with
// CABOOSE_VM_E2E_LIVE=0).
func TestVolumesCount(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" {
		t.Skip("CABOOSE_VM_E2E names no directory of VM files")
	}
	n := config.MaxVMVolumes
	if c, err := strconv.Atoi(os.Getenv("CABOOSE_VM_E2E_VOLUMES")); err == nil {
		n = c
	}
	work, err := os.MkdirTemp("/tmp", "cvc")
	if err != nil {
		t.Fatal(err)
	}
	share := filepath.Join(work, "share")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	vmdir := vm.Dir(filepath.Join(work, "vm"))
	v := backend.NewVM("e2e-count", vmdir, host{dir: dir, work: work, volSize: 4 << 30})
	v.Self, _ = os.Executable()
	v.Ready = ready
	defer func() {
		if b, err := os.ReadFile(vmdir.Log()); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "volumes-count-vmm.log"), b, 0o644)
		}
		_ = v.Remove()
		os.RemoveAll(work)
	}()
	vols := []backend.Volume{{Name: "docker", Target: "/var/lib/docker"}}
	for i := range n {
		vols = append(vols, backend.Volume{Name: "v" + strconv.Itoa(i), Target: "/vol/v" + strconv.Itoa(i), Live: liveVolumes()})
	}
	start := time.Now()
	if err := v.Create(backend.Spec{Image: "e2e", Hostname: "caboose-e2e", User: "0:0", Volumes: vols,
		Mounts: []backend.Mount{{Source: share, Target: "/work"}}, Labels: []string{"caboose.isolation=vm"}}); err != nil {
		t.Fatalf("create with %d volumes: %v", n, err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
	got, err := backend.Output(v, "sh", "-c", `grep -c ' /vol/v[0-9]* ext4 ' /proc/mounts`)
	step(t, start, "%d volumes asked for, %s mounted (%v); disks: %d", n, got, err, len(strings.Fields(must(backend.Output(v, "ls", "/sys/block")))))
	if got != strconv.Itoa(n) {
		t.Errorf("%s of %d volumes mounted", got, n)
	}
	if err := v.Stop(); err != nil {
		t.Fatal(err)
	}
}

func must(s string, _ error) string { return s }

// liveVolumes is whether the tests put volumes on caboose-vmm's own
// device, as the launcher does from macOS 27: unless CABOOSE_VM_E2E_LIVE
// is 0.
func liveVolumes() bool { return os.Getenv("CABOOSE_VM_E2E_LIVE") != "0" }
