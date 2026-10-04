//go:build linux

// Command init is the guest kernel's smoke test: make vm-kernel-smoke
// boots vm-dist/kernel-ARCH under QEMU with this as /init (in an
// initramfs vm.WriteInitramfs writes, as the launcher's is) and reads its
// console. It checks what the sandbox and dockerd rely on and the
// decisions the kernel was built to, prints one line per check and a last
// line "SMOKE PASS" or "SMOKE FAIL", and powers the machine off.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

const afVsock = 40 // AF_VSOCK

func main() {
	// Re-run in new namespaces, below: unshare(CLONE_NEWUSER) refuses a
	// process with threads, which every Go program is.
	if len(os.Args) > 1 && os.Args[1] == "ns" {
		ns()
		return
	}
	failed := 0
	check := func(name string, err error) {
		if err != nil {
			failed++
			fmt.Printf("FAIL %s: %v\n", name, err)
		} else {
			fmt.Printf("ok   %s\n", name)
		}
	}

	check("mount /proc", mount("proc", "/proc", "proc", ""))
	check("mount /sys", mount("sysfs", "/sys", "sysfs", ""))
	check("mount /dev (devtmpfs)", mount("devtmpfs", "/dev", "devtmpfs", ""))
	check("mount /tmp (tmpfs)", mount("tmpfs", "/tmp", "tmpfs", ""))
	check("mount cgroup2", mount("cgroup2", "/sys/fs/cgroup", "cgroup2", ""))
	check("mount securityfs", mount("securityfs", "/sys/kernel/security", "securityfs", ""))

	check("uname -r ends in -caboose", func() error {
		var u syscall.Utsname
		if err := syscall.Uname(&u); err != nil {
			return err
		}
		r := cstr(u.Release[:])
		fmt.Printf("     release %s\n", r)
		if !strings.HasSuffix(r, "-caboose") {
			return fmt.Errorf("release %q", r)
		}
		return nil
	}())
	check("lockdown is integrity", contains("/sys/kernel/security/lockdown", "[integrity]"))
	check("no /dev/mem", absent("/dev/mem"))
	check("no modules", absent("/proc/modules"))
	check("the clock was set from the RTC", func() error {
		// RTC_HCTOSYS sets it before init runs; without an RTC it is 1970.
		now := time.Now().UTC()
		name, _ := os.ReadFile("/sys/class/rtc/rtc0/name")
		fmt.Printf("     %s, rtc0 %s\n", now.Format(time.RFC3339), strings.TrimSpace(string(name)))
		if now.Year() < 2025 {
			return fmt.Errorf("the time is %s", now.Format(time.RFC3339))
		}
		return nil
	}())
	check("io_uring is ENOSYS", func() error {
		// io_uring_setup, the same number on every architecture.
		_, _, e := syscall.Syscall(425, 1, 0, 0)
		if e != syscall.ENOSYS {
			return fmt.Errorf("io_uring_setup: %v, want ENOSYS", e)
		}
		return nil
	}())
	check("cgroup2 controllers", func() error {
		b, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
		if err != nil {
			return err
		}
		have := strings.Fields(string(b))
		for _, c := range []string{"cpuset", "cpu", "io", "memory", "pids"} {
			if !slices.Contains(have, c) {
				return fmt.Errorf("no %s in %q", c, strings.TrimSpace(string(b)))
			}
		}
		return nil
	}())
	check("overlay mount", overlay())
	check("socket(AF_VSOCK)", func() error {
		fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM, 0)
		if err == nil {
			syscall.Close(fd)
		}
		return err
	}())
	check("open /dev/net/tun", func() error {
		f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
		if err == nil {
			f.Close()
		}
		return err
	}())
	check("user, pid, net and mount namespaces", func() error {
		cmd := exec.Command("/init", "ns")
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID |
				syscall.CLONE_NEWNET | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS |
				syscall.CLONE_NEWIPC,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: 0, Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: 0, Size: 1}},
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, bytes.TrimSpace(out))
		}
		return nil
	}())
	if b, _ := os.ReadFile("/proc/cmdline"); strings.Contains(string(b), "ip=dhcp") {
		// The kernel's own DHCP client answered: it writes what it got here.
		check("ip=dhcp configured", contains("/proc/net/pnp", "nameserver"))
	}

	if failed > 0 {
		fmt.Printf("SMOKE FAIL: %d checks failed\n", failed)
	} else {
		fmt.Println("SMOKE PASS")
	}
	syscall.Sync()
	syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	select {}
}

// ns runs in the new namespaces: pid 1 of its own, a loopback of its own,
// and root only inside.
func ns() {
	if os.Getpid() != 1 {
		fmt.Printf("pid %d in the new pid namespace, want 1", os.Getpid())
		os.Exit(1)
	}
	if os.Getuid() != 0 {
		m, _ := os.ReadFile("/proc/self/uid_map")
		fmt.Printf("uid %d in the user namespace, want 0 (uid_map %q)", os.Getuid(), m)
		os.Exit(1)
	}
	if err := syscall.Sethostname([]byte("inner")); err != nil {
		fmt.Printf("sethostname in the uts namespace: %v", err)
		os.Exit(1)
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		fmt.Printf("socket in the net namespace: %v", err)
		os.Exit(1)
	}
	syscall.Close(fd)
}

func mount(src, dst, fstype, data string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return syscall.Mount(src, dst, fstype, 0, data)
}

func overlay() error {
	for _, d := range []string{"/tmp/lower", "/tmp/upper", "/tmp/work", "/tmp/merged"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile("/tmp/lower/a", []byte("lower"), 0o644); err != nil {
		return err
	}
	if err := syscall.Mount("overlay", "/tmp/merged", "overlay", 0,
		"lowerdir=/tmp/lower,upperdir=/tmp/upper,workdir=/tmp/work"); err != nil {
		return err
	}
	if err := os.WriteFile("/tmp/merged/a", []byte("upper"), 0o644); err != nil {
		return err
	}
	if b, err := os.ReadFile("/tmp/upper/a"); err != nil || string(b) != "upper" {
		return fmt.Errorf("the write did not land in upper: %q, %v", b, err)
	}
	if b, _ := os.ReadFile("/tmp/lower/a"); string(b) != "lower" {
		return errors.New("the write reached lower")
	}
	return nil
}

func contains(path, want string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !strings.Contains(string(b), want) {
		return fmt.Errorf("%s is %q, want %q in it", path, strings.TrimSpace(string(b)), want)
	}
	return nil
}

func absent(path string) error {
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %v, want it absent", path, err)
	}
	return nil
}

func cstr[T int8 | uint8](b []T) string {
	s := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		s = append(s, byte(c))
	}
	return string(s)
}
