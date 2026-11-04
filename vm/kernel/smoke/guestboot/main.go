//go:build linux

// Command guestboot checks what a vm guest's real init leaves behind, with
// no hook in that init. make vm-guest-smoke boots it under QEMU two ways:
//
//   - Stage a, as /init in the initramfs: PID 1 runs agent.Init, the very
//     code the launcher's guest runs, which copies /init to the new root and
//     starts it with "guest". So the "agent" is this program again, on the
//     root and console a real one has. It checks and powers off.
//   - Stage b, as the boot spec's entrypoint of a real caboose-agent, from
//     the root disk: it checks and makes the ready file, as the sandbox's
//     entrypoint does. With "no-ready" it never makes it, for a driver to
//     see a boot that never gets ready.
//
// The console's "GUESTBOOT PASS" or "GUESTBOOT FAIL" says how it went.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agent"
)

// grownBytes is what the root's filesystem must offer: more than the 64 MiB
// the scratch filesystem is made with on its 512 MiB disk (vm/kernel/smoke/guest/boot).
const grownBytes = 256 << 20

func main() {
	if os.Getpid() == 1 {
		os.Exit(agent.Init())
	}
	failed := check()
	if failed == 0 {
		fmt.Println("GUESTBOOT PASS")
	} else {
		fmt.Printf("GUESTBOOT FAIL %d\n", failed)
	}
	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch {
	case mode == "guest":
		syscall.Sync()
		// Lets the console drain.
		time.Sleep(200 * time.Millisecond)
		_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	case failed != 0:
		os.Exit(1)
	case mode != "no-ready":
		if err := os.WriteFile("/run/ready", nil, 0o644); err != nil {
			fmt.Println("GUESTBOOT FAIL the ready file:", err)
			os.Exit(1)
		}
	}
	// A sleep, not an empty select: the Go runtime kills a program whose
	// every goroutine is blocked.
	time.Sleep(time.Hour)
}

// check prints a line per check and returns how many failed.
func check() (failed int) {
	report := func(name string, err error) {
		if err != nil {
			failed++
			fmt.Printf("FAIL %s: %v\n", name, err)
		} else {
			fmt.Printf("ok   %s\n", name)
		}
	}
	info, err := os.ReadFile("/proc/self/mountinfo")
	report("read mountinfo", err)
	for _, want := range [][2]string{
		{"/", "overlay"},
		{"/run", "tmpfs"},
		{"/proc", "proc"},
		{"/dev", "devtmpfs"},
		{"/sys", "sysfs"},
		{"/sys/kernel/security", "securityfs"},
		{"/sys/fs/cgroup", "cgroup2"},
		{"/dev/pts", "devpts"},
		{"/dev/shm", "tmpfs"},
	} {
		report("mount "+want[0]+" "+want[1], mounted(string(info), want[0], want[1]))
	}
	for _, l := range [][2]string{
		{"/dev/ptmx", "pts/ptmx"},
		{"/dev/fd", "/proc/self/fd"},
		{"/dev/stdin", "/proc/self/fd/0"},
		{"/dev/stdout", "/proc/self/fd/1"},
		{"/dev/stderr", "/proc/self/fd/2"},
	} {
		got, err := os.Readlink(l[0])
		if err == nil && got != l[1] {
			err = fmt.Errorf("points at %q", got)
		}
		report("link "+l[0], err)
	}
	_, err = os.ReadDir("/dev/fd/")
	report("list /dev/fd/", err)
	report("lo up", loUp())
	report("scratch grew", scratchGrew())
	report("bash process substitution", processSubstitution())
	return failed
}

// mounted looks for path mounted with the file system fs in mountinfo's text.
func mounted(info, path, fs string) error {
	for _, l := range strings.Split(info, "\n") {
		f := strings.Fields(l)
		if len(f) < 9 || f[4] != path {
			continue
		}
		for i, w := range f {
			if w == "-" && i+1 < len(f) && f[i+1] == fs {
				return nil
			}
		}
	}
	return fmt.Errorf("not mounted")
}

func loUp() error {
	b, err := os.ReadFile("/sys/class/net/lo/flags")
	if err != nil {
		return err
	}
	var flags uint
	if _, err := fmt.Sscanf(string(b), "0x%x", &flags); err != nil {
		return fmt.Errorf("flags %q: %w", b, err)
	}
	if flags&syscall.IFF_UP == 0 {
		return fmt.Errorf("flags %q", b)
	}
	return nil
}

// scratchGrew reads the overlay's size, its upper's: the scratch disk's.
func scratchGrew() error {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return err
	}
	if size := st.Blocks * uint64(st.Bsize); size < grownBytes {
		return fmt.Errorf("%d bytes, want at least %d", size, grownBytes)
	}
	return nil
}

// processSubstitution opens /dev/fd as bash does. Builtins only: the root
// holds no other program.
func processSubstitution() error {
	out, err := exec.Command("/bash-static", "-c", "read x < <(echo subst); echo $x").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "subst" {
		return fmt.Errorf("got %q", out)
	}
	return nil
}
