package agent

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/agentproto"
)

// A volume's resize, the guest's half: the host drives it a step at a
// time over the control port (agentproto's VolumeOp) and changes the
// disk's size itself, through caboose-vmm. Growing, the filesystem grows
// online once the disk has (EXT4_IOC_RESIZE_FS, as at a boot). Shrinking,
// the volume is unmounted first -- refused while any process holds it --
// then checked and shrunk with the image's own e2fsck and resize2fs, then
// the host lowers the disk, then the volume is mounted again. The
// filesystem is never shrunk while it is mounted.

// volumeTools is where the image's e2fsck and resize2fs are looked for
// after PATH, which may leave the sbin directories out. The image check
// looks in the same places.
const volumeTools = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// shrinkTools are the image's tools a shrink runs (e2fsprogs).
var shrinkTools = []string{"e2fsck", "resize2fs"}

// trimWait bounds how long a trim of a volume may hold up its unmount,
// and the trims of all the disks a shutdown.
const trimWait = 8 * time.Second

// volumeOps is what a volume's steps do to the guest: the tests stand in
// for all of it.
type volumeOps struct {
	disks diskOps
	// devSize is a block device's size in bytes.
	devSize func(dev string) (int64, error)
	// statfs is a mounted filesystem's size and use, in bytes.
	statfs func(path string) (size, used int64, err error)
	// mounts is the guest's mount table: each mount's target.
	mounts func() ([]string, error)
	// unmount unmounts target.
	unmount func(target string) error
	// holders are the processes that hold a file, a directory or a
	// mapping on the filesystem mounted at target.
	holders func(target string) ([]string, error)
	// trim discards the free blocks of the filesystem mounted at target
	// (FITRIM), so that the host gives their space back.
	trim func(target string) error
	// exclusive says whether dev can be opened exclusively: no filesystem
	// holds it, in any mount namespace.
	exclusive func(dev string) error
	// look finds one of the image's tools.
	look func(name string) (string, error)
	// run runs one of the image's tools to its end, however long it takes:
	// a filesystem tool stopped halfway leaves the filesystem halfway.
	run func(name string, args ...string) toolRun
	// sizeWait bounds how long a step waits for the guest to see the
	// disk's new size.
	sizeWait time.Duration
	// freezer freezes and thaws a volume's filesystem for a checkpoint;
	// sameFS says whether the one mounted at a target is on dev.
	freezer *freezer
	sameFS  func(dev, target string) error
}

// toolRun is how a tool's run went.
type toolRun struct {
	Out string
	// Started: the tool ran. Code is its exit status, -1 when it did not
	// exit (a signal ended it).
	Started bool
	Code    int
	Err     error
}

// guestVolumes is the guest's own.
var guestVolumes = volumeOps{
	disks:   guestDisks,
	devSize: devSize,
	statfs:  fsUsage,
	mounts:  func() ([]string, error) { return mountTargets("/proc/self/mounts") },
	unmount: func(target string) error { return unix.Unmount(target, 0) },
	holders: func(target string) ([]string, error) {
		var st unix.Stat_t
		if err := unix.Stat(target, &st); err != nil {
			return nil, err
		}
		return volumeHolders("/proc", st.Dev)
	},
	trim:      fitrim,
	exclusive: openExclusive,
	look:      lookTool,
	run:       runTool,
	sizeWait:  10 * time.Second,
	freezer:   guestFreezer,
	sameFS:    sameFS,
}

// Volume is VolumeGuest's: one step of a volume's resize, on the boot's
// disk named op.Name.
func (m *machine) Volume(op agentproto.VolumeOp) agentproto.VolumeInfo {
	spec := m.linkable()
	if spec == nil {
		return agentproto.VolumeInfo{Op: op.Op, Error: "the sandbox has not booted yet"}
	}
	return guestVolumes.step(spec.Disks, op)
}

// step does op on the disk of ds it names.
func (o volumeOps) step(ds []agentproto.GuestDisk, op agentproto.VolumeOp) agentproto.VolumeInfo {
	info := agentproto.VolumeInfo{Op: op.Op}
	var disk *agentproto.GuestDisk
	checkpoint := op.Op == agentproto.VolumeFreeze || op.Op == agentproto.VolumeThaw
	for i := range ds {
		switch {
		case ds[i].Serial != "" && ds[i].Serial == op.Name && (!checkpoint || ds[i].Target == op.Target):
			disk = &ds[i]
		case checkpoint && ds[i].Serial == "" && ds[i].Device != "" && op.Target != "" && ds[i].Target == op.Target:
			// One of the framework's own disks, which a checkpoint
			// freezes too: found by where the boot mounted it.
			disk = &ds[i]
		}
	}
	if disk == nil {
		if checkpoint {
			info.Error = fmt.Sprintf("this VM has no volume %q at %q", op.Name, op.Target)
		} else {
			info.Error = fmt.Sprintf("this VM has no volume %q on caboose-vmm's disks", op.Name)
		}
		info.Error = bound(info.Error)
		return info
	}
	info.Target = disk.Target
	if op.Op == agentproto.VolumeThaw {
		// Nothing may stand between a thaw and its filesystem: not the
		// device's lookup, nor the volume's usage.
		o.thawStep(&info, op)
		return info
	}
	dev := disk.Device
	var err error
	if disk.Serial != "" {
		dev, err = o.disks.bySerial(disk.Serial, time.Second)
	}
	if err != nil {
		info.Error = err.Error()
		return info
	}
	if err := o.do(dev, &info, op); err != nil {
		info.Error = err.Error()
	}
	info.Error = bound(info.Error)
	info.Note = bound(info.Note)
	return info
}

// bound keeps the end of s, within MaxVolumeText bytes, cut where a
// character starts.
func bound(s string) string {
	if len(s) <= agentproto.MaxVolumeText {
		return s
	}
	i := len(s) - agentproto.MaxVolumeText + 3
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return "..." + s[i:]
}

// do is step's work, on the volume's device dev, filling info.
func (o volumeOps) do(dev string, info *agentproto.VolumeInfo, op agentproto.VolumeOp) error {
	target := info.Target
	if err := o.usage(dev, info); err != nil {
		return err
	}
	switch op.Op {
	case agentproto.VolumeUsage:
		info.Missing = o.missing()
		return nil
	case agentproto.VolumeGrow:
		if !info.Mounted {
			return fmt.Errorf("%s is not mounted", target)
		}
		if err := o.waitSize(dev, op.Size); err != nil {
			return err
		}
		if err := o.disks.grow(dev, target); err != nil {
			return err
		}
	case agentproto.VolumeUnmount:
		if !info.Mounted {
			return nil
		}
		held, err := o.held(target)
		if err != nil {
			return fmt.Errorf("cannot tell what holds %s: %v", target, err)
		}
		if len(held) > 0 {
			info.Holders = held[:min(len(held), agentproto.MaxVolumeHolders)]
			info.HoldersMore = len(held) - len(info.Holders)
			return fmt.Errorf("%s is in use", target)
		}
		// The discards ext4 has queued are dropped once an unmount
		// starts: a trim first gives the host the space of what was
		// deleted just before.
		if err := o.trimWithin(target, trimWait); err != nil {
			info.Note = "its free space was not trimmed (" + err.Error() + "): the host may keep some of it allocated"
		}
		if err := o.unmount(target); err != nil {
			if errors.Is(err, unix.EBUSY) {
				return fmt.Errorf("unmounting %s: it is busy, held by something the scan of /proc does not see (a process in a mount namespace of its own, or a new one)", target)
			}
			return fmt.Errorf("unmounting %s: %w", target, err)
		}
		// Unmounted here, the filesystem may still be mounted in another
		// mount namespace (a container in the guest that bind-mounts it),
		// and written there: then the device cannot be had exclusively.
		if err := o.exclusive(dev); err != nil {
			msg := fmt.Sprintf("%s is unmounted here, but its filesystem is still in use (%v): most likely mounted in another mount namespace, such as a container in the sandbox with the volume bind-mounted into it; stop that and run this again", target, err)
			if merr := o.remount(dev, target); merr != nil {
				msg += fmt.Sprintf("; mounting it again here failed (%v): 'caboose restart' mounts it", merr)
			} else {
				msg += "; it is mounted again"
			}
			if uerr := o.usage(dev, info); uerr != nil {
				info.Mounted = false
			}
			return errors.New(msg)
		}
	case agentproto.VolumeShrinkFS:
		if info.Mounted {
			return fmt.Errorf("%s is still mounted: a filesystem is never shrunk mounted", target)
		}
		if op.Size < 1<<30 || op.Size > info.DiskBytes {
			return fmt.Errorf("a size of %d bytes for a disk of %d", op.Size, info.DiskBytes)
		}
		return o.shrinkFS(dev, op.Size, info)
	case agentproto.VolumeFreeze:
		if !info.Mounted {
			return fmt.Errorf("%s is not mounted", target)
		}
		if err := o.sameFS(dev, target); err != nil {
			return err
		}
		return o.freezer.Freeze(target, op.Token)
	case agentproto.VolumeMount:
		if info.Mounted {
			return nil
		}
		if err := o.remount(dev, target); err != nil {
			return err
		}
		if err := o.waitSize(dev, op.Size); err != nil {
			info.Note = "mounted, its filesystem left at its size: " + err.Error()
		} else if err := o.disks.grow(dev, target); err != nil {
			info.Note = "mounted, its filesystem left at its size: " + err.Error()
		}
	default:
		return fmt.Errorf("no volume step %q", op.Op)
	}
	return o.usage(dev, info)
}

// thawStep is a thaw: of the filesystem at info.Target, which op's token
// froze.
func (o volumeOps) thawStep(info *agentproto.VolumeInfo, op agentproto.VolumeOp) {
	expired, why, err := o.freezer.Thaw(info.Target, op.Token)
	info.Expired, info.Note = expired, bound(why)
	if err != nil {
		info.Error = bound(err.Error())
	}
}

// remount mounts the volume's filesystem at target again, as a boot does.
func (o volumeOps) remount(dev, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	return o.disks.mount(dev, target, "ext4", 0, diskMountData)
}

// missing names the shrink's tools the image lacks, "" when it has both.
func (o volumeOps) missing() string {
	var no []string
	for _, t := range shrinkTools {
		if _, err := o.look(t); err != nil {
			no = append(no, t)
		}
	}
	return strings.Join(no, ", ")
}

// trimDisks trims the filesystems of the kept disks ds (the volumes and
// dockerd's storage) before a power-off, within wait, and says what
// failed: the discards ext4 had queued, for what was deleted last, are
// lost with the machine, and their space would stay allocated on the
// host. The scratch disk is deleted at the stop, and needs none.
func trimDisks(ds []agentproto.GuestDisk, trim func(string) error, wait time.Duration, logf func(format string, args ...any)) {
	var targets []string
	for _, d := range ds {
		targets = append(targets, d.Target)
	}
	if len(targets) == 0 {
		return
	}
	if err := trimAll(targets, trim, wait); err != nil {
		logf("trimming the disks before powering off: %v (the host may keep some of their free space allocated)", err)
	}
}

// trimWithin trims the filesystem at target, giving up waiting after
// wait: a trim cannot be stopped, and runs on alone.
func (o volumeOps) trimWithin(target string, wait time.Duration) error {
	return trimAll([]string{target}, o.trim, wait)
}

// trimAll trims each filesystem of targets in turn, and says what failed,
// or that wait passed first.
func trimAll(targets []string, trim func(string) error, wait time.Duration) error {
	done := make(chan error, 1)
	go func() {
		var errs []error
		for _, t := range targets {
			if err := trim(t); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", t, err))
			}
		}
		done <- errors.Join(errs...)
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(wait):
		return fmt.Errorf("the trim did not end in %v", wait)
	}
}

// usage fills info with what the volume is now.
func (o volumeOps) usage(dev string, info *agentproto.VolumeInfo) error {
	n, err := o.devSize(dev)
	if err != nil {
		return err
	}
	info.DiskBytes = n
	targets, err := o.mounts()
	if err != nil {
		return err
	}
	info.Mounted, info.FSBytes, info.UsedBytes = false, 0, 0
	for _, t := range targets {
		if t == info.Target {
			info.Mounted = true
		}
	}
	if info.Mounted {
		if info.FSBytes, info.UsedBytes, err = o.statfs(info.Target); err != nil {
			return err
		}
	}
	return nil
}

// held is what keeps target from being unmounted: the mounts inside it,
// then the processes that hold it.
func (o volumeOps) held(target string) ([]string, error) {
	targets, err := o.mounts()
	if err != nil {
		return nil, err
	}
	var held []string
	for _, t := range targets {
		if t != target && strings.HasPrefix(t, target+"/") {
			held = append(held, "mount at "+t)
		}
	}
	procs, err := o.holders(target)
	return append(held, procs...), err
}

// waitSize waits for the guest to see dev at size bytes: it hears of a
// new size from the device a moment after the host changed it. 0 waits
// for nothing.
func (o volumeOps) waitSize(dev string, size int64) error {
	if size == 0 {
		return nil
	}
	var n int64
	for deadline := time.Now().Add(o.sizeWait); ; time.Sleep(20 * time.Millisecond) {
		var err error
		if n, err = o.devSize(dev); err != nil {
			return err
		}
		if n == size || time.Now().After(deadline) {
			break
		}
	}
	if n != size {
		return fmt.Errorf("the guest sees the disk at %d bytes, not %d, after %v", n, size, o.sizeWait)
	}
	return nil
}

// shrinkFS checks the unmounted filesystem on dev and shrinks it to size
// bytes. e2fsck runs with -f, which resize2fs asks for, and -p: it fixes
// only what is safe to fix unattended, and stops (exit 4) at anything
// else, where -y would answer yes to every question, data-losing repairs
// included, on the user's files. -E discard has it discard the free
// blocks once its check found nothing wrong, which the host turns into
// space given back: the discards the mounted filesystem had queued were
// dropped at its unmount. Neither tool is ever stopped halfway; one that
// ended halfway (a signal), or a resize2fs that failed, may have left the
// filesystem half changed, which is never mounted again (Dirty).
func (o volumeOps) shrinkFS(dev string, size int64, info *agentproto.VolumeInfo) error {
	r := o.run("e2fsck", "-f", "-p", "-E", "discard", dev)
	switch {
	case !r.Started:
		return fmt.Errorf("e2fsck: %v", r.Err)
	case r.Code < 0:
		info.Dirty = true
		return fmt.Errorf("e2fsck -f -p %s did not exit (%v), and may have left the filesystem half repaired: %s", dev, r.Err, lastLines(r.Out, 5))
	case r.Code > 2:
		return fmt.Errorf("e2fsck -f -p %s exited %d, and nothing was shrunk (it fixes only what is safe unattended, and stops at anything else): %s", dev, r.Code, lastLines(r.Out, 5))
	}
	k := strconv.FormatInt(size>>10, 10) + "K"
	r = o.run("resize2fs", dev, k)
	switch {
	case !r.Started:
		return fmt.Errorf("resize2fs: %v", r.Err)
	case r.Code < 0:
		info.Dirty = true
		return fmt.Errorf("resize2fs %s %s did not exit (%v), and may have left the filesystem half shrunk: %s", dev, k, r.Err, lastLines(r.Out, 5))
	case r.Code != 0:
		info.Dirty = true
		return fmt.Errorf("resize2fs %s %s exited %d, and may have left the filesystem half shrunk: %s", dev, k, r.Code, lastLines(r.Out, 5))
	}
	info.Note = lastLines(r.Out, 2)
	return nil
}

// lastLines is the last n lines of s, joined.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// toolDirs is where the image's tools are looked for: PATH's
// directories, then volumeTools'.
func toolDirs() []string {
	var dirs []string
	seen := map[string]bool{}
	for _, d := range filepath.SplitList(os.Getenv("PATH") + ":" + volumeTools) {
		if filepath.IsAbs(d) && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// lookTool finds one of the image's tools in toolDirs.
func lookTool(name string) (string, error) {
	for _, d := range toolDirs() {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("the image has no %s (on PATH, or in %s): e2fsprogs has it", name, volumeTools)
}

// runTool runs one of the image's tools, found by lookTool, with its
// output combined, to its end.
func runTool(name string, args ...string) toolRun {
	p, err := lookTool(name)
	if err != nil {
		return toolRun{Code: -1, Err: err}
	}
	var out bytes.Buffer
	cmd := exec.Command(p, args...)
	cmd.Env = []string{"PATH=" + strings.Join(toolDirs(), ":")}
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	r := toolRun{Out: out.String(), Code: -1, Err: err}
	if cmd.ProcessState != nil {
		r.Started, r.Code = true, cmd.ProcessState.ExitCode()
	}
	return r
}

// fitrim is FITRIM on the filesystem mounted at target: every free block
// of it discarded.
func fitrim(target string) error {
	f, err := os.Open(target)
	if err != nil {
		return err
	}
	defer f.Close()
	r := fstrimRange{Len: ^uint64(0)}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fiTrim, uintptr(unsafe.Pointer(&r))); e != 0 {
		return fmt.Errorf("FITRIM: %w", e)
	}
	return nil
}

// fstrimRange is struct fstrim_range, FITRIM's argument.
type fstrimRange struct{ Start, Len, MinLen uint64 }

// fiTrim is FITRIM, _IOWR('X', 121, struct fstrim_range).
const fiTrim = 0xc0185879

// openExclusive opens the block device dev with O_EXCL, which fails with
// EBUSY while a mounted filesystem holds it, in any mount namespace.
func openExclusive(dev string) error {
	fd, err := unix.Open(dev, unix.O_RDONLY|unix.O_EXCL|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

// devSize is BLKGETSIZE64 of dev.
func devSize(dev string) (int64, error) {
	f, err := os.Open(dev)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var size uint64
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), blkGetSize64, uintptr(unsafe.Pointer(&size))); e != 0 {
		return 0, fmt.Errorf("%s: its size: %w", dev, e)
	}
	return int64(size), nil
}

// fsUsage is the size and use of the filesystem mounted at path.
func fsUsage(path string) (size, used int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize)
	return int64(st.Blocks) * bs, int64(st.Blocks-st.Bfree) * bs, nil
}

// mountTargets are the targets of the mount table at path (/proc/self/
// mounts), unescaped.
func mountTargets(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 1 {
			out = append(out, unescapeMount(f[1]))
		}
	}
	return out, nil
}

// unescapeMount undoes the kernel's octal escapes (\040 for a space) in a
// mount table's field.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// volumeHolders are the processes under proc (/proc) holding the
// filesystem of device dev (the volume's, mounted): an open file, a
// working or root directory, an executable or a mapping on it. A
// process's paths are its own, so the device tells, not the path: one in
// a mount namespace of its own (a container in the guest) counts too.
// Each holder is "PID (COMMAND): PATH", once per process.
func volumeHolders(proc string, dev uint64) ([]string, error) {
	ents, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}
	self := strconv.Itoa(os.Getpid())
	var out []string
	for _, e := range ents {
		pid := e.Name()
		if _, err := strconv.Atoi(pid); err != nil || pid == self {
			continue
		}
		dir := filepath.Join(proc, pid)
		if p, ok := holds(dir, dev); ok {
			comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
			out = append(out, fmt.Sprintf("%s (%s): %s", pid, printable(strings.TrimSpace(string(comm))), printable(p)))
		}
	}
	return out, nil
}

// holds says whether the process whose /proc directory is dir holds a
// file on the filesystem of device dev, and which.
func holds(dir string, dev uint64) (string, bool) {
	on := func(link string) (string, bool) {
		var st unix.Stat_t
		if unix.Stat(link, &st) != nil || st.Dev != dev {
			return "", false
		}
		p, err := os.Readlink(link)
		if err != nil {
			p = link
		}
		return p, true
	}
	for _, l := range []string{"cwd", "root", "exe"} {
		if p, ok := on(filepath.Join(dir, l)); ok {
			return p, true
		}
	}
	fds, _ := os.ReadDir(filepath.Join(dir, "fd"))
	for _, fd := range fds {
		if p, ok := on(filepath.Join(dir, "fd", fd.Name())); ok {
			return p, true
		}
	}
	return mapsOn(filepath.Join(dir, "maps"), dev)
}

// mapsOn finds a mapping of the maps file at path on device dev: its
// fourth field is the device, MAJOR:MINOR in hex.
func mapsOn(path string, dev uint64) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	want := fmt.Sprintf("%02x:%02x", unix.Major(dev), unix.Minor(dev))
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) >= 6 && fs[3] == want && fs[4] != "0" {
			return strings.Join(fs[5:], " "), true
		}
	}
	return "", false
}
