package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/vm"
)

// TestVolumeResize boots a VM with a volume holding data, as the launcher
// creates a sandbox, and resizes it while it runs, through
// backend.VM.ResizeVolume, as `caboose volume resize` does: grown while a
// writer writes to it; a shrink refused below what it holds, and refused
// while a process holds a file and has its directory there, naming it;
// then shrunk, its data intact, its filesystem clean, its file cut on the
// Mac. With CABOOSE_VM_E2E_LIVE=0 the volume is on the framework's own
// disk, and a resize is refused, naming macOS 27. Its VM's files go in a
// directory of DIR.
func TestVolumeResize(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" {
		t.Skip("CABOOSE_VM_E2E names no directory of VM files")
	}
	work, err := os.MkdirTemp(dir, "cvr")
	if err != nil {
		t.Fatal(err)
	}
	h := host{dir: dir, work: work, volSize: 4 << 30, sizes: map[string]int64{"cache": 8 << 30}}
	vmdir := vm.Dir(filepath.Join(work, "vm"))
	v := backend.NewVM("e2e-resize", vmdir, h)
	v.Self, _ = os.Executable()
	v.Ready = ready
	disk := filepath.Join(work, "volumes", "cache.img")
	defer func() {
		for _, f := range []string{vmdir.Log(), vmdir.Console()} {
			if b, err := os.ReadFile(f); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "resize-"+filepath.Base(f)), b, 0o644)
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
	say := func(format string, args ...any) { step(t, start, "  caboose: "+format, args...) }
	live := liveVolumes()
	if err := v.Create(backend.Spec{
		Image: "e2e", Hostname: "caboose-e2e", User: "0:0",
		Volumes: []backend.Volume{{Name: "docker", Target: "/var/lib/docker"}, {Name: "cache", Target: "/vol/cache", Live: live}},
		Labels:  []string{"caboose.isolation=vm"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
	step(t, start, "ready; e2fsck %s, resize2fs %s", out(`command -v e2fsck || echo none`), out(`command -v resize2fs || echo none`))
	if !live {
		_, err := v.ResizeVolume(context.Background(), "cache", 12<<30, say)
		step(t, start, "on the framework's disk: %v", err)
		if err == nil || !strings.Contains(err.Error(), "macOS 27") {
			t.Errorf("a resize of a volume on the framework's disk: %v", err)
		}
		return
	}
	dev := out(`awk '$2 == "/vol/cache" {print $1}' /proc/mounts`)
	state := func(when string) (sectors, fsKiB int64) {
		t.Helper()
		sectors, _ = strconv.ParseInt(out(`cat /sys/block/`+strings.TrimPrefix(dev, "/dev/")+`/size`), 10, 64)
		fsKiB, _ = strconv.ParseInt(out(`df -k /vol/cache | tail -1 | awk '{print $2}'`), 10, 64)
		var st syscall.Stat_t
		_ = syscall.Stat(disk, &st)
		step(t, start, "%s: %s %d sectors (%d GiB), file system %d KiB, used %s KiB; on the Mac %d bytes, %d MiB allocated",
			when, dev, sectors, sectors>>21, fsKiB, out(`df -k /vol/cache | tail -1 | awk '{print $3}'`), st.Size, st.Blocks*512>>20)
		return sectors, fsKiB
	}
	sum := out(`dd if=/dev/urandom of=/vol/cache/data bs=1M count=512 2>/dev/null; sync; sha256sum < /vol/cache/data`)
	step(t, start, "wrote 512 MiB: %s", sum)
	state("before")

	// Grown while a writer writes, each write synced.
	out(`touch /run/go; setsid sh -c 'i=0; while [ -e /run/go ]; do dd if=/dev/urandom of=/vol/cache/w$((i%8)) bs=1M count=16 conv=fsync 2>/dev/null || echo BAD; i=$((i+1)); echo $i > /run/writes; done; echo done >> /run/writer.log' >>/run/writer.log 2>&1 </dev/null &`)
	time.Sleep(time.Second)
	res, err := v.ResizeVolume(context.Background(), "cache", 12<<30, say)
	step(t, start, "grow to 12G: %+v, %v", res, err)
	if err != nil || !res.Applied {
		t.Fatalf("grow: %+v, %v", res, err)
	}
	time.Sleep(time.Second)
	out(`rm /run/go; sleep 2`)
	step(t, start, "the writer: %s writes, log %q", out(`cat /run/writes`), out(`cat /run/writer.log`))
	if log := out(`cat /run/writer.log`); strings.Contains(log, "BAD") || !strings.Contains(log, "done") {
		t.Errorf("the writer: %q", log)
	}
	sectors, fsKiB := state("grown")
	if sectors*512 != 12<<30 || fsKiB*1024 < (12<<30)*9/10 || fileSize(t, disk) != 12<<30 {
		t.Errorf("grown: %d sectors, %d KiB, file %d", sectors, fsKiB, fileSize(t, disk))
	}
	out(`rm -f /vol/cache/w*; sync`)

	// Below what it holds, plus the margin: refused, nothing changed.
	_, err = v.ResizeVolume(context.Background(), "cache", 1<<30, say)
	step(t, start, "shrink to 1G: %v", err)
	if err == nil || !strings.Contains(err.Error(), "can go down to") {
		t.Errorf("a shrink below what it holds: %v", err)
	}

	// Held: a process with its directory and a file open there.
	out(`setsid sh -c 'cd /vol/cache && exec 3</vol/cache/data && exec sleep 600' >/dev/null 2>&1 </dev/null & echo $! > /run/holder`)
	time.Sleep(500 * time.Millisecond)
	_, err = v.ResizeVolume(context.Background(), "cache", 6<<30, say)
	step(t, start, "shrink while held: %v", err)
	if err == nil || !strings.Contains(err.Error(), "(sleep): /vol/cache") || !strings.Contains(err.Error(), "nothing changed") {
		t.Errorf("a shrink while held: %v", err)
	}
	if out(`awk '$2 == "/vol/cache"' /proc/mounts | wc -l`) != "1" {
		t.Error("the volume is not mounted after a refused shrink")
	}
	out(`kill $(cat /run/holder); sleep 0.5`)

	// A file past the new end, for the shrink to move: the space below
	// filled first, the filler deleted after (discard gives it back).
	out(`i=0; while [ $i -lt 13 ]; do dd if=/dev/zero of=/vol/cache/fill$i bs=1M count=512 2>/dev/null; i=$((i+1)); done; sync`)
	high := out(`dd if=/dev/urandom of=/vol/cache/high bs=1M count=512 2>/dev/null; sync; sha256sum < /vol/cache/high`)
	step(t, start, "high file: %s; its extents: %s", high, out(`(command -v filefrag >/dev/null && filefrag -v /vol/cache/high | sed -n 4p) || echo "no filefrag"`))
	full := allocated(t, disk)
	out(`rm -f /vol/cache/fill*; sync`)
	// The mount's discard gives the filler's space back, a moment after.
	freed := settle(t, disk, full, 13*512<<20)
	state("filler deleted")
	step(t, start, "filler deleted: %d MiB of its 6656 MiB given back on the Mac", freed>>20)
	if freed < 13*512<<20*9/10 {
		t.Errorf("deleting the filler gave back %d MiB of 6656", freed>>20)
	}

	var before syscall.Stat_t
	_ = syscall.Stat(disk, &before)
	t0 := time.Now()
	res, err = v.ResizeVolume(context.Background(), "cache", 6<<30, say)
	step(t, start, "shrink to 6G in %v: %+v, %v", time.Since(t0).Round(time.Millisecond), res, err)
	if err != nil || !res.Applied {
		t.Fatalf("shrink: %+v, %v", res, err)
	}
	sectors, fsKiB = state("shrunk")
	var after syscall.Stat_t
	_ = syscall.Stat(disk, &after)
	step(t, start, "allocated on the Mac: %d MiB before the shrink, %d MiB after", before.Blocks*512>>20, after.Blocks*512>>20)
	if sectors*512 != 6<<30 || fsKiB*1024 > 6<<30 || fsKiB*1024 < (6<<30)*8/10 || after.Size != 6<<30 {
		t.Errorf("shrunk: %d sectors, %d KiB, file %d", sectors, fsKiB, after.Size)
	}
	if _, err := os.Stat(disk + backend.BackupSuffix); !os.IsNotExist(err) {
		t.Errorf("the backup is still there: %v", err)
	}
	again := out(`echo 3 > /proc/sys/vm/drop_caches; sha256sum < /vol/cache/data; sha256sum < /vol/cache/high`)
	if again != sum+"\n"+high {
		t.Errorf("the data: %q, written %q and %q", again, sum, high)
	}
	if after.Blocks >= before.Blocks {
		t.Errorf("allocated on the Mac did not drop: %d blocks, %d before", after.Blocks, before.Blocks)
	}
	// What the shrink frees comes back too: e2fsck -E discard, after the
	// trim before the unmount. What stays is the files, 1 GiB, and the
	// filesystem's own metadata.
	used, _ := strconv.ParseInt(out(`df -k /vol/cache | tail -1 | awk '{print $3}'`), 10, 64)
	if extra := after.Blocks*512 - used<<10; extra > 384<<20 {
		t.Errorf("allocated on the Mac after the shrink: %d MiB, %d MiB more than the filesystem uses", after.Blocks*512>>20, extra>>20)
	}
	// Deleted just before a shrink, with no time for the mount's discards
	// to land: the unmount would drop them, and the trim before it and
	// e2fsck -E discard after it give the space back anyway.
	out(`dd if=/dev/zero of=/vol/cache/late bs=1M count=2048 2>/dev/null; sync`)
	lateFull := allocated(t, disk)
	out(`rm -f /vol/cache/late; sync`)
	res, err = v.ResizeVolume(context.Background(), "cache", 5<<30, say)
	step(t, start, "deleted 2 GiB, then shrunk to 5G at once: %+v, %v", res, err)
	if err != nil || !res.Applied {
		t.Fatalf("shrink: %+v, %v", res, err)
	}
	used, _ = strconv.ParseInt(out(`df -k /vol/cache | tail -1 | awk '{print $3}'`), 10, 64)
	late := allocated(t, disk)
	step(t, start, "allocated on the Mac: %d MiB with the 2 GiB file, %d MiB after the shrink, the filesystem using %d MiB", lateFull>>20, late>>20, used>>10)
	if extra := late - used<<10; extra > 384<<20 {
		t.Errorf("allocated on the Mac after a delete and a shrink at once: %d MiB, %d MiB more than the filesystem uses", late>>20, extra>>20)
	}
	sectors, fsKiB = state("shrunk again")
	if sectors*512 != 5<<30 || fileSize(t, disk) != 5<<30 {
		t.Errorf("shrunk again: %d sectors, %d KiB, file %d", sectors, fsKiB, fileSize(t, disk))
	}
	// The guest's image may have no mount or umount: the agent's own steps
	// unmount and mount it around e2fsck.
	c, err := vmdir.Control(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if info, err := c.Volume(agentproto.VolumeOp{Op: agentproto.VolumeUnmount, Name: "cache"}, time.Minute); err != nil || info.Error != "" {
		t.Fatalf("unmount: %+v, %v", info, err)
	}
	fsck := out(`e2fsck -fn ` + dev + `; echo "exit $?"`)
	step(t, start, "e2fsck -fn after: %s", strings.ReplaceAll(fsck, "\n", " / "))
	if !strings.HasSuffix(fsck, "exit 0") {
		t.Errorf("e2fsck: %s", fsck)
	}
	if info, err := c.Volume(agentproto.VolumeOp{Op: agentproto.VolumeMount, Name: "cache", Size: 5 << 30}, time.Minute); err != nil || info.Error != "" || !info.Mounted {
		t.Errorf("mount: %+v, %v", info, err)
	}
	step(t, start, "errors in dmesg: %q", out(`dmesg | grep -i -E 'error|fail|EXT4-fs (warning|error)|virtio_blk' | tail -8; true`))
	log, _ := os.ReadFile(vmdir.Log())
	for _, l := range strings.Split(string(log), "\n") {
		if strings.Contains(l, "disk \"cache\"") || strings.Contains(l, "capacity") {
			t.Logf("vmm.log: %s", l)
		}
	}
}

// settle waits up to 20 s for path's allocation, from, to drop by want
// bytes, and says how much it dropped.
func settle(t *testing.T, path string, from, want int64) int64 {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		freed := from - allocated(t, path)
		if freed >= want || time.Now().After(deadline) {
			return freed
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestVolumeStopTrim deletes a volume's file and stops the VM at once: the
// discards the guest had queued for it would go with the machine, and the
// agent's trim of the kept disks before the power-off gives the space
// back. The same for dockerd's disk. Its VM's files go in a directory of
// DIR.
func TestVolumeStopTrim(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" {
		t.Skip("CABOOSE_VM_E2E names no directory of VM files")
	}
	work, err := os.MkdirTemp(dir, "cvt")
	if err != nil {
		t.Fatal(err)
	}
	h := host{dir: dir, work: work, volSize: 8 << 30}
	vmdir := vm.Dir(filepath.Join(work, "vm"))
	v := backend.NewVM("e2e-stoptrim", vmdir, h)
	v.Self, _ = os.Executable()
	v.Ready = ready
	defer func() {
		if b, err := os.ReadFile(vmdir.Console()); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "stoptrim-console.log"), b, 0o644)
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
	if err := v.Create(backend.Spec{
		Image: "e2e", Hostname: "caboose-e2e", User: "0:0",
		Volumes: []backend.Volume{{Name: "docker", Target: "/var/lib/docker"}, {Name: "cache", Target: "/vol/cache", Live: liveVolumes()}},
		Labels:  []string{"caboose.isolation=vm"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
	disks := map[string]string{"/vol/cache": filepath.Join(work, "volumes", "cache.img"), "/var/lib/docker": filepath.Join(work, "volumes", "docker.img")}
	full := map[string]int64{}
	for target, img := range disks {
		out(`i=0; while [ $i -lt 4 ]; do dd if=/dev/zero of=` + target + `/f$i bs=1M count=512 2>/dev/null; i=$((i+1)); done; sync`)
		full[target] = allocated(t, img)
	}
	out(`rm -f /vol/cache/f* /var/lib/docker/f*; sync`)
	t0 := time.Now()
	if err := v.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	step(t, start, "deleted 2 GiB on each disk, then stopped in %v", time.Since(t0).Round(time.Millisecond))
	for target, img := range disks {
		freed := full[target] - allocated(t, img)
		step(t, start, "%s: %d MiB allocated before the delete, %d MiB after the stop: %d MiB of 2048 given back", target, full[target]>>20, allocated(t, img)>>20, freed>>20)
		if freed < 2048<<20*9/10 {
			t.Errorf("%s: %d MiB of 2048 given back", target, freed>>20)
		}
	}
	if b, err := os.ReadFile(vmdir.Console()); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, "trim") {
				step(t, start, "console: %s", l)
			}
		}
	}
}
