package agent

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/agentproto"
)

// The disks the VM has, in the order vmm attaches them.
const (
	// rootDisk is the image, flattened to ext4, and never written.
	rootDisk = "/dev/vda"
	// scratchDisk is an empty ext4 at every start: the overlay's upper,
	// unless the command line says agentproto.ScratchTmpfs.
	scratchDisk = "/dev/vdb"
)

// guestAgent is where the init copies itself on the new root, to run as
// the agent from once the initramfs is out of reach.
const guestAgent = agentproto.GuestAgent

// Init is caboose-agent as a vm guest's PID 1, /init in the initramfs the
// launcher writes. It makes the root (the image read-only, under an
// overlay on the scratch disk), moves into it, and from then on only runs
// `caboose-agent guest` -- again if it dies -- and reaps: every orphan in
// the guest comes to PID 1, and exec.Cmd would race a reaper in the
// process that uses it. It never returns.
func Init() int {
	log := os.Stderr // the console
	// PID 1 gets no signal it has no handler for, but Go's runtime has
	// one for each, which would end it (and panic the kernel).
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGPIPE, syscall.SIGUSR1, syscall.SIGUSR2)
	fmt.Fprintf(log, "caboose-agent: init: started %v after the kernel\n", sinceBoot())
	if err := earlyInit(); err != nil {
		fmt.Fprintf(log, "caboose-agent: init: %v; powering off\n", err)
		time.Sleep(time.Second)
		unix.Sync()
		_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
		select {}
	}
	fmt.Fprintf(log, "caboose-agent: init: the root is ready %v after the kernel\n", sinceBoot())
	var agent int
	for {
		if agent == 0 {
			pid, err := syscall.ForkExec(guestAgent, []string{guestAgent, "guest"}, &syscall.ProcAttr{
				Env:   []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
				Files: []uintptr{0, 1, 2},
			})
			if err != nil {
				fmt.Fprintf(log, "caboose-agent: init: starting the agent: %v\n", err)
				time.Sleep(time.Second)
				continue
			}
			agent = pid
		}
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if pid == agent {
			fmt.Fprintf(log, "caboose-agent: init: the agent exited (%s); starting it again\n", describeWait(ws))
			agent = 0
			time.Sleep(time.Second)
		}
	}
}

// sinceBoot is the monotonic clock, which in a guest starts at its
// kernel's start: the boot's times on the console are all on it, so a
// run can tell the kernel's, the init's, the host's and the entrypoint's
// shares apart.
func sinceBoot() time.Duration {
	var ts unix.Timespec
	if unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts) != nil {
		return 0
	}
	return time.Duration(ts.Nano()).Round(time.Millisecond)
}

func describeWait(ws unix.WaitStatus) string {
	if ws.Signaled() {
		return "signal " + ws.Signal().String()
	}
	return fmt.Sprintf("status %d", ws.ExitStatus())
}

// earlyInit takes the guest from the initramfs to the sandbox's root.
func earlyInit() error {
	for _, d := range []string{"/dev", "/proc", "/mnt/lower", "/mnt/scratch", "/mnt/root"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := mount("devtmpfs", "/dev", "devtmpfs", 0, "mode=0755"); err != nil {
		return err
	}
	if err := mount("proc", "/proc", "proc", 0, ""); err != nil {
		return err
	}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return err
	}
	tmpfsScratch := scratchOnTmpfs(string(cmdline))
	disks := []string{rootDisk}
	if !tmpfsScratch {
		disks = append(disks, scratchDisk)
	}
	for _, d := range disks {
		if err := waitForFile(d, 5*time.Second); err != nil {
			return err
		}
	}
	if err := mount(rootDisk, "/mnt/lower", "ext4", unix.MS_RDONLY, ""); err != nil {
		return err
	}
	if tmpfsScratch {
		err = mount("tmpfs", "/mnt/scratch", "tmpfs", 0, "mode=0755")
	} else {
		err = mount(scratchDisk, "/mnt/scratch", "ext4", 0, "")
	}
	if err != nil {
		return err
	}
	for _, d := range []string{"/mnt/scratch/upper", "/mnt/scratch/work"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := mount("overlay", "/mnt/root", "overlay", 0,
		"lowerdir=/mnt/lower,upperdir=/mnt/scratch/upper,workdir=/mnt/scratch/work"); err != nil {
		return err
	}
	if err := os.MkdirAll("/mnt/root/run", 0o755); err != nil {
		return err
	}
	if err := mount("tmpfs", "/mnt/root/run", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755"); err != nil {
		return err
	}
	if err := copyFile("/init", "/mnt/root"+guestAgent, 0o755); err != nil {
		return err
	}
	for _, d := range []string{"/dev", "/proc"} {
		if err := os.MkdirAll("/mnt/root"+d, 0o755); err != nil {
			return err
		}
		if err := mount(d, "/mnt/root"+d, "", unix.MS_MOVE, ""); err != nil {
			return err
		}
	}
	// switch_root: the initramfs cannot be pivoted away from, so the new
	// root is moved over / and entered.
	if err := unix.Chdir("/mnt/root"); err != nil {
		return err
	}
	if err := mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return err
	}
	if err := unix.Chroot("."); err != nil {
		return err
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	return lateMounts(os.Stderr)
}

// scratchOnTmpfs reports whether the kernel's command line puts the
// overlay's upper on tmpfs (caboose.scratch=tmpfs), leaving every disk
// after the root to the guest: the builder's, whose disks are its cache
// and its output.
func scratchOnTmpfs(cmdline string) bool {
	for _, f := range strings.Fields(cmdline) {
		if f == agentproto.ScratchTmpfs {
			return true
		}
	}
	return false
}

// lateMount is one of lateMounts'.
type lateMount struct {
	src, dst, fs string
	flags        uintptr
	data         string
	// optional is a mount the guest boots without, when the kernel has
	// no such file system: said on the console, never a failed boot.
	optional bool
}

// lateMountTable is what lateMounts mounts, in order. securityfs is where
// the kernel says its lockdown mode (and any LSM's state); a kernel built
// without it (Kata's, say) still boots.
var lateMountTable = []lateMount{
	{src: "sysfs", dst: "/sys", fs: "sysfs", flags: unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
	{src: "securityfs", dst: "/sys/kernel/security", fs: "securityfs", flags: unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, optional: true},
	{src: "cgroup2", dst: "/sys/fs/cgroup", fs: "cgroup2", flags: unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
	{src: "devpts", dst: "/dev/pts", fs: "devpts", flags: unix.MS_NOSUID | unix.MS_NOEXEC, data: "newinstance,ptmxmode=0666,mode=0620,gid=5"},
	{src: "shm", dst: "/dev/shm", fs: "tmpfs", flags: unix.MS_NOSUID | unix.MS_NODEV, data: "mode=1777"},
}

// lateMounts are the rest of what a container runtime mounts, on the new
// root, and the loopback interface up; log is the console.
func lateMounts(log io.Writer) error {
	for _, m := range lateMountTable {
		err := os.MkdirAll(m.dst, 0o755)
		if err == nil {
			err = mount(m.src, m.dst, m.fs, m.flags, m.data)
		}
		if err != nil && m.optional {
			fmt.Fprintf(log, "caboose-agent: init: no %s: %v\n", m.fs, err)
			continue
		}
		if err != nil {
			return err
		}
	}
	// The terminals openPTY makes are this devpts's.
	_ = os.Remove("/dev/ptmx")
	if err := os.Symlink("pts/ptmx", "/dev/ptmx"); err != nil {
		return err
	}
	return interfaceUp("lo")
}

func mount(src, dst, fs string, flags uintptr, data string) error {
	if err := unix.Mount(src, dst, fs, flags, data); err != nil {
		return fmt.Errorf("mounting %s at %s: %w", src, dst, err)
	}
	return nil
}

func waitForFile(path string, timeout time.Duration) error {
	for deadline := time.Now().Add(timeout); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("no %s: %w", path, err)
		}
	}
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// interfaceUp sets a network interface up (the kernel gives lo its
// 127.0.0.1; ip=dhcp brings up and configures eth0).
func interfaceUp(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("%s up: %w", name, err)
	}
	return nil
}
