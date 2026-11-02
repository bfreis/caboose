package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/agentproto"
)

// diskMountData is the data every writable ext4 disk of the guest is
// mounted with, so that the blocks a deleted file frees go back to the
// host: the virtual disk is a sparse file there, and a discard punches a
// hole in it.
const diskMountData = "discard"

const (
	// blkGetSize64 is BLKGETSIZE64: a block device's size in bytes.
	blkGetSize64 = 0x80081272
	// ext4IocResizeFS is EXT4_IOC_RESIZE_FS: grow a mounted ext4 to the
	// number of blocks the u64 argument holds.
	ext4IocResizeFS = 0x40086610
)

// ext4Geometry reads an ext4 superblock, the 1024 bytes at offset 1024 of
// its device: how many blocks of how many bytes the filesystem has.
func ext4Geometry(sb []byte) (blocks, blockSize uint64, err error) {
	if len(sb) < 0x160 {
		return 0, 0, errors.New("a short superblock")
	}
	if binary.LittleEndian.Uint16(sb[0x38:]) != 0xEF53 {
		return 0, 0, errors.New("not an ext4 superblock")
	}
	shift := binary.LittleEndian.Uint32(sb[0x18:])
	if shift > 6 {
		return 0, 0, fmt.Errorf("a block size of 1024<<%d", shift)
	}
	blocks = uint64(binary.LittleEndian.Uint32(sb[0x04:]))
	// The high half is there only on a 64bit filesystem (incompat 0x80).
	if binary.LittleEndian.Uint32(sb[0x60:])&0x80 != 0 {
		blocks |= uint64(binary.LittleEndian.Uint32(sb[0x150:])) << 32
	}
	return blocks, 1024 << shift, nil
}

// growBlocks is the size, in blocks, the filesystem should be grown to
// fill a device of devBytes, and false when it already does (or the
// device is the shorter one, which a grow cannot fix).
func growBlocks(fsBlocks, blockSize, devBytes uint64) (uint64, bool) {
	if blockSize == 0 {
		return 0, false
	}
	n := devBytes / blockSize
	return n, n > fsBlocks
}

// growFS grows the ext4 of dev, mounted at mnt, to fill dev: the host
// makes a disk by making a small empty ext4 longer, and never formats or
// reads one. An online grow adds groups to the filesystem as it is, and
// needs no tool.
func growFS(dev, mnt string) error {
	f, err := os.Open(dev)
	if err != nil {
		return err
	}
	defer f.Close()
	var size uint64
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), blkGetSize64, uintptr(unsafe.Pointer(&size))); e != 0 {
		return fmt.Errorf("%s: its size: %w", dev, e)
	}
	sb := make([]byte, 1024)
	if _, err := f.ReadAt(sb, 1024); err != nil {
		return fmt.Errorf("%s: its superblock: %w", dev, err)
	}
	blocks, bs, err := ext4Geometry(sb)
	if err != nil {
		return fmt.Errorf("%s: %w", dev, err)
	}
	n, grow := growBlocks(blocks, bs, size)
	if !grow {
		return nil
	}
	dir, err := os.Open(mnt)
	if err != nil {
		return err
	}
	defer dir.Close()
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, dir.Fd(), ext4IocResizeFS, uintptr(unsafe.Pointer(&n))); e != 0 {
		return fmt.Errorf("growing %s from %d to %d blocks: %w", mnt, blocks, n, e)
	}
	return nil
}

// diskOps is what mounting a writable disk does to the guest: find it,
// mount it, with diskMountData, and grow its filesystem to fill it. The
// tests stand in for all of it.
type diskOps struct {
	mount func(src, dst, fs string, flags uintptr, data string) error
	grow  func(dev, mnt string) error
	// sysBlock is where the kernel lists the guest's disks (/sys/block),
	// and dev where their nodes are (/dev).
	sysBlock, dev string
}

// guestDisks is the guest's own.
var guestDisks = diskOps{mount: mount, grow: growFS, sysBlock: "/sys/block", dev: "/dev"}

// diskWait bounds how long a boot waits for a disk to show up.
var diskWait = 5 * time.Second

// mountScratch mounts the scratch disk dev at mnt and grows its
// filesystem. A grow that fails fails the boot, saying what to change:
// the disk is as long as scratch_size asks, and a sandbox given less would
// fill up short of it with no word why.
func (o diskOps) mountScratch(dev, mnt string) error {
	if err := o.mount(dev, mnt, "ext4", 0, diskMountData); err != nil {
		return err
	}
	if err := o.grow(dev, mnt); err != nil {
		return fmt.Errorf("the scratch disk's filesystem did not grow to fill it (%v): "+
			"a smaller scratch_size in the [vm.NAME] profile of config.toml, or none for the default, may boot", err)
	}
	return nil
}

// mountDisks mounts each volume's ext4 disk at its target, making the
// target, and grows its filesystem to fill the disk. A grow that fails is
// said, and the volume used at the size it has: what it holds is what the
// sandbox kept, which a failed boot would keep from it. A disk is named
// by its serial (one of caboose-vmm's, from macOS 27) or by its device
// (one of the framework's: dockerd's storage, and the volumes before
// macOS 27). The VM's user is root (launcher's isolation.go), so the
// targets made, and a new filesystem's root, stay root's.
func (o diskOps) mountDisks(ds []agentproto.GuestDisk, logf func(format string, args ...any)) error {
	for _, d := range ds {
		dev := d.Device
		if d.Serial != "" {
			var err error
			if dev, err = o.bySerial(d.Serial, diskWait); err != nil {
				return err
			}
		} else if err := waitForFile(dev, diskWait); err != nil {
			return err
		}
		if err := os.MkdirAll(d.Target, 0o755); err != nil {
			return err
		}
		if err := o.mount(dev, d.Target, "ext4", 0, diskMountData); err != nil {
			return fmt.Errorf("mounting %s at %s: %w", dev, d.Target, err)
		}
		if err := o.grow(dev, d.Target); err != nil {
			logf("the volume at %s keeps its filesystem's size: %v", d.Target, err)
		}
	}
	return nil
}

// bySerial is the node of the disk whose serial is serial, waiting up to
// wait for it to show up: caboose-vmm's disks are named in the order the
// guest finds them, which nothing fixes, and are told apart by their
// serial alone.
func (o diskOps) bySerial(serial string, wait time.Duration) (string, error) {
	for deadline := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
		names, err := o.serials(serial)
		if err != nil {
			return "", err
		}
		switch {
		case len(names) == 1:
			return filepath.Join(o.dev, names[0]), nil
		case len(names) > 1:
			return "", fmt.Errorf("the disks %s all have serial %q", strings.Join(names, ", "), serial)
		case time.Now().After(deadline):
			return "", fmt.Errorf("no disk with serial %q (in %s)", serial, o.sysBlock)
		}
	}
}

// serials are the disks of sysBlock whose serial is serial.
func (o diskOps) serials(serial string) ([]string, error) {
	ents, err := os.ReadDir(o.sysBlock)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(o.sysBlock, e.Name(), "serial"))
		if err == nil && strings.TrimSpace(string(b)) == serial {
			names = append(names, e.Name())
		}
	}
	return names, nil
}
