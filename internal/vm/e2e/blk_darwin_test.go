package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/vm"
)

// blkSerial is the serial of the block tests' device.
const blkSerial = "caboose-blk-0"

// sparseFile makes a sparse file of size at path, over any there.
func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	_ = os.Remove(path)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}

// blkVM is a VM booted with block devices of caboose-vmm's, and what the
// tests run in it.
type blkVM struct {
	t     *testing.T
	v     *backend.VM
	dir   vm.Dir
	start time.Time
}

// bootBlk boots a VM with bds, its logs kept in DIR under name.
func bootBlk(t *testing.T, dir, name string, bds []vm.BlockDevice) *blkVM {
	t.Helper()
	work, err := os.MkdirTemp("/tmp", "cvm")
	if err != nil {
		t.Fatal(err)
	}
	vmdir := vm.Dir(filepath.Join(work, "vm"))
	v := backend.NewVM("e2e", vmdir, host{dir: dir, work: work, blk: bds})
	v.Self, _ = os.Executable()
	v.Ready = ready
	b := &blkVM{t: t, v: v, dir: vmdir, start: time.Now()}
	t.Cleanup(func() {
		for _, f := range []string{vmdir.Log(), vmdir.Console()} {
			if out, err := os.ReadFile(f); err == nil {
				base := strings.TrimSuffix(filepath.Base(f), ".log")
				_ = os.WriteFile(filepath.Join(dir, base+"-"+name+".log"), out, 0o644)
			}
		}
		_ = v.Remove()
		os.RemoveAll(work)
	})
	if err := v.Create(backend.Spec{Image: "e2e", Hostname: "caboose-e2e", User: "0:0", Labels: []string{"caboose.isolation=vm"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
	b.step("ready")
	if _, err := backend.Capture(v, backend.ExecSpec{Argv: []string{"sh", "-c", "mkdir -p /run/bin && cat > /run/bin/mount && chmod +x /run/bin/mount && ln -sf mount /run/bin/umount"}, Stdin: true}, strings.NewReader(guestMount)); err != nil {
		t.Fatalf("installing mount: %v", err)
	}
	return b
}

func (b *blkVM) step(format string, args ...any) {
	b.t.Helper()
	step(b.t, b.start, format, args...)
}

// vmmLog logs vmm's lines about its block devices.
func (b *blkVM) vmmLog(prefix string) {
	out, _ := os.ReadFile(b.dir.Log())
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "blk") {
			b.t.Logf("%s vmm.log: %s", prefix, l)
		}
	}
}

// run runs a script in the guest, bounded, its stderr with its stdout,
// with mount and umount from guestMount first on PATH.
func (b *blkVM) run(secs int, script string) (string, error) {
	return backend.Output(b.v, "timeout", strconv.Itoa(secs), "sh", "-c", "PATH=/run/bin:$PATH; "+script+" 2>&1")
}

// sh is run, failing the test on an error. A script that times out is a
// stalled disk: what the guest and vmm say of it ends the test.
func (b *blkVM) sh(script string) string {
	b.t.Helper()
	s, err := b.run(60, script)
	if err != nil {
		b.t.Errorf("%s: %v\n%s", script, err, s)
		if strings.Contains(err.Error(), "cannot reach") {
			b.vmmLog("the VM is gone:")
			b.t.FailNow()
		}
		if strings.Contains(err.Error(), "status 124") {
			g, _ := backend.Output(b.v, "sh", "-c", "for d in /sys/block/vd*; do echo $d inflight $(cat $d/inflight); done; dmesg | tail -5")
			b.t.Logf("stalled; the guest:\n%s", g)
			b.vmmLog("stalled:")
			b.t.FailNow()
		}
	}
	return s
}

// dev is the guest's node for the disk of serial, by its serial alone.
func (b *blkVM) dev(serial string) string {
	b.t.Helper()
	d := b.sh(`for d in /sys/block/vd*; do [ "$(cat $d/serial 2>/dev/null)" = "` + serial + `" ] && echo ${d##*/}; done`)
	if d == "" {
		b.step("dmesg:\n%s", b.sh(`dmesg | grep -i -E 'virtio|vd[a-z]|blk' | tail -30`))
		b.vmmLog("")
		b.t.Fatalf("no disk with serial %s", serial)
	}
	return d
}

// sectors is the size the guest sees of its disk dev.
func (b *blkVM) sectors(dev string) int64 {
	n, _ := strconv.ParseInt(b.sh("cat /sys/block/"+dev+"/size"), 10, 64)
	return n
}

// fsKiB is the size of the file system mounted at mnt.
func (b *blkVM) fsKiB(mnt string) int64 {
	n, _ := strconv.ParseInt(b.sh(`df -k `+mnt+` | tail -1 | awk '{print $2}'`), 10, 64)
	return n
}

// stop stops the VM, and checks vmm handed back every request.
func (b *blkVM) stop() {
	b.t.Helper()
	t0 := time.Now()
	if err := b.v.Stop(); err != nil {
		b.t.Fatalf("stop: %v", err)
	}
	b.step("stopped in %.2fs", time.Since(t0).Seconds())
	out, _ := os.ReadFile(b.dir.Log())
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, ": stop with") {
			b.t.Logf("vmm.log: %s", l)
			if !strings.Contains(l, " 0 left unreturned") || !strings.Contains(l, " 0 dropped") {
				b.t.Errorf("a stop left requests: %s", l)
			}
		}
	}
}

// fileSize is the size of the file at path on the Mac.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// TestBlockDevice boots a VM with one of caboose-vmm's own block devices,
// over a sparse file in DIR, and checks what the guest makes of it: that
// it binds as a virtio-blk disk, of the size the machine gives it (a
// smaller file grown to it); that a file system on it keeps its data with
// many requests in flight, through a dropped cache and a remount; that a
// trim gives space back to the Mac; and that a boot at a larger size
// grows the file, and the mounted file system grows into it
// (EXT4_IOC_RESIZE_FS) with its data intact. The guest's image needs
// mkfs.ext4, python3 and sha256sum.
func TestBlockDevice(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" {
		t.Skip("CABOOSE_VM_E2E names no directory of VM files")
	}
	img := filepath.Join(dir, "blk.img")
	sparseFile(t, img, 1<<30)
	b := bootBlk(t, dir, "blk", []vm.BlockDevice{{Path: img, Serial: blkSerial, Size: 2 << 30}})
	b.step("tools: %s", b.sh(`for c in mkfs.ext4 python3 sha256sum; do echo -n "$(command -v $c || echo no-$c) "; done`))
	dev := b.dev(blkSerial)
	b.step("the device is /dev/%s; dmesg:\n%s", dev, b.sh(`dmesg | grep -E '`+dev+`|virtio_blk' | tail -10`))
	b.step("queue limits: %s", b.sh(`cd /sys/block/`+dev+`/queue; for f in max_segments max_segment_size max_hw_sectors_kb max_sectors_kb discard_max_bytes discard_granularity write_zeroes_max_bytes write_cache; do echo -n "$f=$(cat $f) "; done`))
	b.vmmLog("after boot")
	if got := b.sectors(dev); got != 2<<30/512 {
		t.Errorf("the guest sees %d sectors, want %d", got, 2<<30/512)
	}
	if got := fileSize(t, img); got != 2<<30 {
		t.Errorf("the file is %d bytes, not grown to the device's 2 GiB", got)
	}

	// sums checks every /mnt/blk file against the sum of its source
	// (/run/src<N>, by the file name's last digit).
	const sums = `bad=0; n=0; for f in /mnt/blk/w*; do n=$((n+1)); s=/run/src$(( ${f##*[!0-9]} % 8 )); [ "$(sha256sum < $f)" = "$(cat $s.sum)" ] || { bad=$((bad+1)); echo "MISMATCH $f"; }; done; echo "$n files, $bad bad"`
	sources := `for i in 0 1 2 3 4 5 6 7; do [ -e /run/src$i ] || { dd if=/dev/urandom of=/run/src$i bs=1M count=16 2>/dev/null; sha256sum < /run/src$i > /run/src$i.sum; }; done; ls /run | grep -c src`
	b.step("sources: %s", b.sh(sources))
	b.step("mkfs: %s", b.sh(`mkfs.ext4 -q -F /dev/`+dev+` && mkdir -p /mnt/blk && mount /dev/`+dev+` /mnt/blk && df -k /mnt/blk | tail -1`))

	// The pool under load: 32 direct writers, then 8 buffered ones, at
	// once, then readers of everything.
	t0 := time.Now()
	b.step("parallel writes: %s", b.sh(`for i in $(seq 0 31); do dd if=/run/src$((i%8)) of=/mnt/blk/w$i bs=256k oflag=direct 2>/dev/null & done; for i in $(seq 32 39); do dd if=/run/src$((i%8)) of=/mnt/blk/w$i bs=1M conv=fsync 2>/dev/null & done; wait; sync; ls /mnt/blk | grep -c w`))
	b.step("640 MiB in %v", time.Since(t0).Round(time.Millisecond))
	out := b.sh(`echo 3 > /proc/sys/vm/drop_caches; ` + sums)
	b.step("after drop_caches: %s", out)
	if !strings.Contains(out, "40 files, 0 bad") {
		t.Errorf("after drop_caches: %s", out)
	}
	out = b.sh(`umount /mnt/blk && mount /dev/` + dev + ` /mnt/blk && ` + sums)
	b.step("after a remount: %s", out)
	if !strings.Contains(out, "40 files, 0 bad") {
		t.Errorf("after a remount: %s", out)
	}

	// A trim gives the space back; one file stays.
	before := allocated(t, img)
	b.step("rm + trim: %s", b.sh(`for i in $(seq 1 39); do rm /mnt/blk/w$i; done; sync; python3 -c 'import fcntl,struct,os
fd=os.open("/mnt/blk",os.O_RDONLY); b=bytearray(struct.pack("QQQ",0,2**64-1,0)); fcntl.ioctl(fd,0xC0185879,b); print("trimmed",struct.unpack("QQQ",b)[1])'`))
	after := allocated(t, img)
	b.step("allocated on the Mac: %.1f -> %.1f MiB", float64(before)/(1<<20), float64(after)/(1<<20))
	if after > before-500<<20 {
		t.Errorf("the trim gave back too little: %d -> %d bytes", before, after)
	}
	w0 := b.sh(`sha256sum < /mnt/blk/w0`)
	b.sh(`umount /mnt/blk`)
	b.stop()

	// A boot at a larger size grows the file, and the guest grows the
	// mounted file system into it.
	b = bootBlk(t, dir, "blk-grown", []vm.BlockDevice{{Path: img, Serial: blkSerial, Size: 3 << 30}})
	dev = b.dev(blkSerial)
	if got := b.sectors(dev); got != 3<<30/512 {
		t.Errorf("after the boot at 3 GiB, the guest sees %d sectors", got)
	}
	if got := fileSize(t, img); got != 3<<30 {
		t.Errorf("the file is %d bytes, not grown to 3 GiB", got)
	}
	b.sh(`mkdir -p /mnt/blk && mount /dev/` + dev + ` /mnt/blk`)
	fs := b.fsKiB("/mnt/blk")
	b.step("online grow: %s", b.sh(`python3 -c 'import fcntl,os,struct
fd=os.open("/mnt/blk",os.O_RDONLY); st=os.statvfs("/mnt/blk")
blocks=int(open("/sys/block/`+dev+`/size").read())*512//st.f_frsize
fcntl.ioctl(fd,0x40086610,struct.pack("Q",blocks)); print("EXT4_IOC_RESIZE_FS to",blocks,"blocks")'`))
	grown := b.fsKiB("/mnt/blk")
	b.step("the file system %d -> %d KiB", fs, grown)
	if grown < fs+900<<10 {
		t.Errorf("the mounted file system did not grow: %d -> %d KiB", fs, grown)
	}
	if got := b.sh(`echo 3 > /proc/sys/vm/drop_caches; sha256sum < /mnt/blk/w0`); got != w0 || w0 == "" {
		t.Errorf("w0 after the grow: %q, before %q", got, w0)
	}
	b.step("errors in dmesg: %s", b.sh(`dmesg | grep -i -E 'error|fail|timeout|hung|EXT4-fs (warning|error)' | tail -5; true`))
	b.sh(`umount /mnt/blk`)
	b.stop()
	b.vmmLog("end")
}

// TestBlockDevicePowerOff powers the guest off at once, with no sync,
// while 32 writers of zeroes keep requests in flight: vmm must stop
// cleanly, with what its log says of the requests left.
func TestBlockDevicePowerOff(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" {
		t.Skip("CABOOSE_VM_E2E names no directory of VM files")
	}
	img := filepath.Join(dir, "blk-poweroff.img")
	sparseFile(t, img, 1<<30)
	b := bootBlk(t, dir, "blk-poweroff", []vm.BlockDevice{{Path: img, Serial: blkSerial}})
	dev := b.dev(blkSerial)
	out, err := backend.Output(b.v, "sh", "-c", `(for i in $(seq 0 31); do dd if=/dev/zero of=/dev/`+dev+` bs=64k seek=$((i*256)) count=100000 oflag=direct 2>/dev/null & done; wait) </dev/null >/dev/null 2>&1 & sleep 1; echo "inflight $(cat /sys/block/`+dev+`/inflight)"`)
	b.step("writers: %s %v", out, err)
	_, err = backend.Output(b.v, "python3", "-c", "import ctypes; ctypes.CDLL(None).reboot(0x4321fedc)")
	b.step("power off: %v", err)
	for t0 := time.Now(); b.v.State() == "running" && time.Since(t0) < 20*time.Second; {
		time.Sleep(50 * time.Millisecond)
	}
	b.step("state %s", b.v.State())
	log, _ := os.ReadFile(b.dir.Log())
	for _, l := range strings.Split(string(log), "\n") {
		if strings.Contains(l, "blk0: reset") || strings.Contains(l, "blk0: stop") || strings.Contains(l, "VM") || strings.Contains(l, "panic") || strings.Contains(l, "exception") {
			t.Logf("vmm.log: %s", l)
		}
	}
	if b.v.State() == "running" {
		t.Error("the VM still runs")
	}
	if !strings.Contains(string(log), "the VM powered off") {
		t.Error("vmm did not see the guest power off")
	}
}

// guestMount is mount DEV DIR (ext4) and umount DIR, for an image with
// neither: python3 calling libc.
const guestMount = `#!/usr/bin/env python3
import ctypes, os, sys
libc = ctypes.CDLL(None, use_errno=True)
if os.path.basename(sys.argv[0]) == "umount":
    r = libc.umount2(sys.argv[1].encode(), 0)
else:
    r = libc.mount(sys.argv[1].encode(), sys.argv[2].encode(), b"ext4", 0, None)
if r != 0:
    e = ctypes.get_errno()
    sys.exit("%s: %s" % (sys.argv[0], os.strerror(e)))
`
