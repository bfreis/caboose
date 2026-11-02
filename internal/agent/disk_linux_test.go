package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

// superblock is 1024 bytes laid out as ext4's: the magic, the block count
// (its high half only with the 64bit feature) and the block size's shift.
func superblock(blocks uint64, shift uint32, sixtyFour bool) []byte {
	sb := make([]byte, 1024)
	binary.LittleEndian.PutUint16(sb[0x38:], 0xEF53)
	binary.LittleEndian.PutUint32(sb[0x04:], uint32(blocks))
	binary.LittleEndian.PutUint32(sb[0x18:], shift)
	if sixtyFour {
		binary.LittleEndian.PutUint32(sb[0x60:], 0x80)
		binary.LittleEndian.PutUint32(sb[0x150:], uint32(blocks>>32))
	}
	return sb
}

func TestExt4Geometry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sb     []byte
		blocks uint64
		bs     uint64
	}{
		{"4 GiB of 4 KiB", superblock(1<<20, 2, true), 1 << 20, 4096},
		{"a count past 32 bits", superblock(5<<32+7, 2, true), 5<<32 + 7, 4096},
		{"1 KiB blocks", superblock(4096, 0, false), 4096, 1024},
		{"high half ignored without 64bit", func() []byte {
			sb := superblock(100, 2, false)
			binary.LittleEndian.PutUint32(sb[0x150:], 9)
			return sb
		}(), 100, 4096},
	} {
		blocks, bs, err := ext4Geometry(tc.sb)
		if err != nil || blocks != tc.blocks || bs != tc.bs {
			t.Errorf("%s: %d blocks of %d, %v", tc.name, blocks, bs, err)
		}
	}
	bad := superblock(1, 2, true)
	binary.LittleEndian.PutUint16(bad[0x38:], 0)
	huge := superblock(1, 9, true)
	for name, sb := range map[string][]byte{"no magic": bad, "short": make([]byte, 100), "a huge block": huge} {
		if _, _, err := ext4Geometry(sb); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// What mke2fs makes is read as it was asked for, when this machine has it.
func TestExt4GeometryOfMke2fs(t *testing.T) {
	mke2fs, err := exec.LookPath("mke2fs")
	if err != nil {
		t.Skip("no mke2fs")
	}
	img := filepath.Join(t.TempDir(), "e.img")
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, 64<<20); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(mke2fs, "-q", "-F", "-t", "ext4", "-b", "4096", img).CombinedOutput(); err != nil {
		t.Skipf("mke2fs: %v: %s", err, out)
	}
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sb := make([]byte, 1024)
	if _, err := f.ReadAt(sb, 1024); err != nil {
		t.Fatal(err)
	}
	blocks, bs, err := ext4Geometry(sb)
	if err != nil || blocks != 64<<20/4096 || bs != 4096 {
		t.Errorf("%d blocks of %d, %v", blocks, bs, err)
	}
}

func TestGrowBlocks(t *testing.T) {
	for _, tc := range []struct {
		name              string
		fs, bs, dev, want uint64
		grow              bool
	}{
		{"a 4 GiB filesystem on a 64 GiB disk", 1 << 20, 4096, 64 << 30, 16 << 20, true},
		{"already full", 1 << 20, 4096, 4 << 30, 1 << 20, false},
		{"a disk with a partial block at its end", 1 << 20, 4096, 4<<30 + 4095, 1 << 20, false},
		{"a disk a block longer", 1 << 20, 4096, 4<<30 + 4096, 1<<20 + 1, true},
		{"a disk shorter than the filesystem", 1 << 20, 4096, 2 << 30, 1 << 19, false},
		{"no block size", 1 << 20, 0, 64 << 30, 0, false},
	} {
		n, grow := growBlocks(tc.fs, tc.bs, tc.dev)
		if grow != tc.grow || (grow && n != tc.want) {
			t.Errorf("%s: %d, %v", tc.name, n, grow)
		}
	}
}

// diskCalls records what diskOps asked of the guest.
type diskCalls struct {
	mounts  []string // "src dst fs data"
	grows   []string // "dev mnt"
	growErr map[string]error
	// sysBlock and dev are the disks the guest has, by serial.
	sysBlock, dev string
}

func (c *diskCalls) ops() diskOps {
	return diskOps{
		mount: func(src, dst, fs string, flags uintptr, data string) error {
			c.mounts = append(c.mounts, src+" "+dst+" "+fs+" "+data)
			return nil
		},
		grow: func(dev, mnt string) error {
			c.grows = append(c.grows, dev+" "+mnt)
			return c.growErr[dev]
		},
		sysBlock: c.sysBlock, dev: c.dev,
	}
}

// sysDisks writes a /sys/block of disks named as serials says, by node
// name ("" for a disk with no serial file), and is that directory.
func sysDisks(t *testing.T, serials map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, serial := range serials {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if serial != "" {
			if err := os.WriteFile(filepath.Join(dir, name, "serial"), []byte(serial+"\n"), 0o444); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// The scratch disk is mounted with discard, so that what the guest
// deletes goes back to the host, and grown; a grow that fails fails the
// boot, naming the key that sets the disk's size.
func TestMountScratch(t *testing.T) {
	c := &diskCalls{}
	if err := c.ops().mountScratch("/dev/vdb", "/mnt/scratch"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/dev/vdb /mnt/scratch ext4 discard"}; !reflect.DeepEqual(c.mounts, want) {
		t.Errorf("mounts %q, want %q", c.mounts, want)
	}
	if want := []string{"/dev/vdb /mnt/scratch"}; !reflect.DeepEqual(c.grows, want) {
		t.Errorf("grows %q, want %q", c.grows, want)
	}
	c = &diskCalls{growErr: map[string]error{"/dev/vdb": errors.New("no space for the group descriptors")}}
	err := c.ops().mountScratch("/dev/vdb", "/mnt/scratch")
	if err == nil || !strings.Contains(err.Error(), "no space for the group descriptors") || !strings.Contains(err.Error(), "a smaller scratch_size in the [vm.NAME] profile of config.toml") {
		t.Errorf("a failed grow: %v", err)
	}
}

// Every volume is mounted with discard and grown; one whose grow fails is
// said and used at the size it has, and the next is still mounted.
func TestMountDisks(t *testing.T) {
	dir := t.TempDir()
	var ds []agentproto.GuestDisk
	for _, n := range []string{"vdc", "vdd"} {
		dev := filepath.Join(dir, n)
		if err := os.WriteFile(dev, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ds = append(ds, agentproto.GuestDisk{Device: dev, Target: filepath.Join(dir, "mnt", n)})
	}
	c := &diskCalls{growErr: map[string]error{ds[0].Device: errors.New("EXT4_IOC_RESIZE_FS: invalid argument")}}
	var said []string
	logf := func(format string, args ...any) { said = append(said, fmt.Sprintf(format, args...)) }
	if err := c.ops().mountDisks(ds, logf); err != nil {
		t.Fatalf("a failed grow failed the boot: %v", err)
	}
	var mounts, grows []string
	for _, d := range ds {
		mounts = append(mounts, d.Device+" "+d.Target+" ext4 discard")
		grows = append(grows, d.Device+" "+d.Target)
		if fi, err := os.Stat(d.Target); err != nil || !fi.IsDir() {
			t.Errorf("no target made at %s: %v", d.Target, err)
		}
	}
	if !reflect.DeepEqual(c.mounts, mounts) {
		t.Errorf("mounts %q, want %q", c.mounts, mounts)
	}
	if !reflect.DeepEqual(c.grows, grows) {
		t.Errorf("grows %q, want %q", c.grows, grows)
	}
	if len(said) != 1 || !strings.Contains(said[0], "the volume at "+ds[0].Target+" keeps its filesystem's size") || !strings.Contains(said[0], "invalid argument") {
		t.Errorf("said %q", said)
	}
}

// The guest's own mounts and grows are the real ones, and its disks are
// the kernel's.
func TestGuestDisks(t *testing.T) {
	if reflect.ValueOf(guestDisks.mount).Pointer() != reflect.ValueOf(mount).Pointer() ||
		reflect.ValueOf(guestDisks.grow).Pointer() != reflect.ValueOf(growFS).Pointer() {
		t.Error("the guest's disks are mounted or grown by something else")
	}
	if guestDisks.sysBlock != "/sys/block" || guestDisks.dev != "/dev" {
		t.Errorf("the guest's disks are looked for in %s and %s", guestDisks.sysBlock, guestDisks.dev)
	}
}

// A volume on one of caboose-vmm's disks is found by its serial, whatever
// node the guest gave it; one on the framework's, by its device, as
// caboose's own disk is; each target is made.
func TestMountDisksBySerial(t *testing.T) {
	sys := sysDisks(t, map[string]string{"vda": "", "vdc": "", "vdd": "data", "vde": "cache"})
	dir := t.TempDir()
	for _, n := range []string{"vdc", "vdf"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ds := []agentproto.GuestDisk{
		{Device: filepath.Join(dir, "vdc"), Target: filepath.Join(dir, "docker", "lib")},
		{Device: filepath.Join(dir, "vdf"), Target: filepath.Join(dir, "home", "old")},
		{Serial: "cache", Target: filepath.Join(dir, "home", "c", "cache")},
		{Serial: "data", Target: filepath.Join(dir, "data")},
	}
	c := &diskCalls{sysBlock: sys, dev: "/dev"}
	if err := c.ops().mountDisks(ds, t.Logf); err != nil {
		t.Fatal(err)
	}
	want := []string{ds[0].Device + " " + ds[0].Target + " ext4 discard", ds[1].Device + " " + ds[1].Target + " ext4 discard",
		"/dev/vde " + ds[2].Target + " ext4 discard", "/dev/vdd " + ds[3].Target + " ext4 discard"}
	if !reflect.DeepEqual(c.mounts, want) {
		t.Errorf("mounts %q, want %q", c.mounts, want)
	}
	for _, d := range ds {
		if fi, err := os.Stat(d.Target); err != nil || !fi.IsDir() {
			t.Errorf("no target made at %s: %v", d.Target, err)
		}
	}
}

// A serial no disk has fails the boot, once the wait is over, and so does
// one two disks have.
func TestMountDisksSerialMissing(t *testing.T) {
	defer func(w time.Duration) { diskWait = w }(diskWait)
	diskWait = 50 * time.Millisecond
	c := &diskCalls{sysBlock: sysDisks(t, map[string]string{"vdc": "data", "vdd": "data"}), dev: "/dev"}
	err := c.ops().mountDisks([]agentproto.GuestDisk{{Serial: "cache", Target: filepath.Join(t.TempDir(), "c")}}, t.Logf)
	if err == nil || !strings.Contains(err.Error(), `no disk with serial "cache"`) {
		t.Errorf("a missing disk: %v", err)
	}
	err = c.ops().mountDisks([]agentproto.GuestDisk{{Serial: "data", Target: filepath.Join(t.TempDir(), "d")}}, t.Logf)
	if err == nil || !strings.Contains(err.Error(), `all have serial "data"`) {
		t.Errorf("a serial twice: %v", err)
	}
	if len(c.mounts) != 0 {
		t.Errorf("mounted %q", c.mounts)
	}
}

// A disk that shows up late is waited for.
func TestMountDisksSerialLate(t *testing.T) {
	sys := t.TempDir()
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.MkdirAll(filepath.Join(sys, "vdc"), 0o755)
		_ = os.WriteFile(filepath.Join(sys, "vdc", "serial"), []byte("late"), 0o444)
	}()
	c := &diskCalls{sysBlock: sys, dev: "/dev"}
	if err := c.ops().mountDisks([]agentproto.GuestDisk{{Serial: "late", Target: filepath.Join(t.TempDir(), "l")}}, t.Logf); err != nil {
		t.Fatal(err)
	}
	if len(c.mounts) != 1 || !strings.HasPrefix(c.mounts[0], "/dev/vdc ") {
		t.Errorf("mounts %q", c.mounts)
	}
}
