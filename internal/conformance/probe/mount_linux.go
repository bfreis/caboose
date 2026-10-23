//go:build linux

package probe

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
)

// mountCall is the one system call of op on target.
func mountCall(op, target string) error {
	switch op {
	case MountBindRW:
		return syscall.Mount("", target, "", syscall.MS_REMOUNT|syscall.MS_BIND, "")
	case MountRW:
		return syscall.Mount("", target, "", syscall.MS_REMOUNT, "")
	case MountBindRO:
		return syscall.Mount("", target, "", syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY, "")
	case Umount:
		return syscall.Unmount(target, 0)
	case UmountDetach:
		return syscall.Unmount(target, syscall.MNT_DETACH)
	}
	return &usageError{fmt.Errorf("%q is not a mount operation the probe knows", op)}
}

// userNS starts a copy of the probe (UserNSChild, through userNSChildArg)
// created in a new user and mount namespace, mapped to root inside it,
// and reports what its attempts came to. The namespace is made by the
// kernel at clone time, for a process of one thread: a Go process cannot
// unshare a user namespace itself (it always runs more than one thread, so
// Linux answers EINVAL), and a refusal for that reason would prove
// nothing. Only a policy refusal of the namespace (EPERM, as a container's
// seccomp gives, or ENOSPC/EUSERS when no user namespaces are allowed)
// counts as held; any other failure to start the copy is an error.
func userNS(work, nonce, target, part, witness string) (held bool, lines []string, err error) {
	var p [2]int
	if e := syscall.Pipe2(p[:], syscall.O_CLOEXEC); e != nil {
		return false, nil, e
	}
	pid, e := syscall.ForkExec(selfExe, []string{selfExe, userNSChildArg, work, nonce, target, part, witness}, &syscall.ProcAttr{
		Files: []uintptr{^uintptr(0), uintptr(p[1]), uintptr(syscall.Stderr)},
		Sys: &syscall.SysProcAttr{
			Cloneflags:                 cloneNewUser | cloneNewNS,
			UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: syscall.Geteuid(), Size: 1}},
			GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: syscall.Getegid(), Size: 1}},
			GidMappingsEnableSetgroups: false,
		},
	})
	_ = syscall.Close(p[1])
	if e != nil {
		_ = syscall.Close(p[0])
		if nsRefused(e) {
			return true, []string{"namespace: " + ErrnoText(e)}, nil
		}
		return false, []string{"namespace: " + ErrnoText(e)}, fmt.Errorf("the namespace could not be made, so nothing was tried: %w", e)
	}
	out := readUpTo(p[0], 4096)
	_ = syscall.Close(p[0])
	var ws syscall.WaitStatus
	_, werr := syscall.Wait4(pid, &ws, 0, nil)
	lines = append(lines, "namespace: ok")
	got := strings.Split(strings.TrimSpace(out), "\n")
	lines = append(lines, got...)
	// A success the copy reported decides, whatever else went wrong.
	for _, l := range got {
		if strings.HasSuffix(l, ": ok") {
			return false, lines, nil
		}
	}
	if werr != nil || !ws.Exited() || ws.ExitStatus() != 0 || len(got) != 3 {
		return false, lines, fmt.Errorf("the copy in the namespace did not report its three attempts (%v, status %v)", werr, ws)
	}
	return true, lines, nil
}

// nsRefused reports whether e, from making the namespace, is a policy
// refusal of it: EPERM (a container's seccomp), EACCES (an LSM such as
// AppArmor's restriction of unprivileged user namespaces), or ENOSPC and
// EUSERS (no user namespaces allowed). Anything else, EINVAL above all,
// proves nothing about the sandbox.
func nsRefused(e error) bool {
	var en syscall.Errno
	return errors.As(e, &en) && (en == syscall.EPERM || en == syscall.EACCES || en == syscall.ENOSPC || en == syscall.EUSERS)
}

// selfExe is the probe's own executable, the one program userNS starts.
const selfExe = "/proc/self/exe"

// readUpTo reads fd until its end or n bytes.
func readUpTo(fd, n int) string {
	b := make([]byte, 0, n)
	buf := make([]byte, 512)
	for len(b) < n {
		k, e := syscall.Read(fd, buf)
		if k > 0 {
			b = append(b, buf[:min(k, n-len(b))]...)
		}
		if e == syscall.EINTR {
			continue
		}
		if e != nil || k <= 0 {
			break
		}
	}
	return string(b)
}

// userNSChildArg is the argument that makes the probe the copy userNS
// starts; Main hands it to UserNSChild before reading any flag.
const userNSChildArg = "--userns-child"

// userNSAttempts is the copy's work, inside the namespace: a bind remount
// of the read-only root without MS_RDONLY, an unmount of the part, and a
// write to a read-only canary. It prints one line per attempt.
func userNSAttempts(target, part, witness string, stdout io.Writer) {
	step := func(name string, e error) {
		if e == nil {
			fmt.Fprintf(stdout, "%s: ok\n", name)
			return
		}
		fmt.Fprintf(stdout, "%s: %s\n", name, ErrnoText(e))
	}
	step("remount-bind-rw "+target, mountCall(MountBindRW, target))
	step("umount "+part, mountCall(Umount, part))
	step("witness "+witness, writeOne(witness))
}

// cloneNewUser and cloneNewNS name the unshare flags, so the one allowed
// spelling of them is here.
const (
	cloneNewUser = syscall.CLONE_NEWUSER
	cloneNewNS   = syscall.CLONE_NEWNS
)

// writeOne appends one byte to an existing file, the witness: a write
// that lands where the root is read-only is the breach. It never creates.
func writeOne(path string) error {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	_, err = syscall.Write(fd, []byte("x"))
	return err
}
