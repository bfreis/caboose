package probe

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// The probe's mount mode: one mount(2) or umount2(2) call on one of the
// suite's own mount points, made by the probe itself so that what is tried
// is the system call and not whatever mount binary the image has (or
// lacks). The sandbox must refuse every one of them: none of its users
// holds CAP_SYS_ADMIN. The probe reports the call's errno as strerror says
// it in the C locale, which the suite classifies as it does the shell's
// messages, and exits ExitRefused; it exits 0 only when the call
// succeeded, which the suite reports as a FAIL.

// The mount operations.
const (
	// MountBindRW remounts the mount point's bind mount without
	// MS_RDONLY: what would make a read-only root writable.
	MountBindRW = "remount-bind-rw"
	// MountRW remounts the mount point's filesystem without MS_RDONLY, as
	// mount -o remount,rw does.
	MountRW = "remount-rw"
	// MountBindRO remounts the bind mount read-only again: harmless in
	// itself, but only a holder of CAP_SYS_ADMIN may make it.
	MountBindRO = "remount-bind-ro"
	// Umount unmounts the mount point.
	Umount = "umount"
	// UmountDetach unmounts it lazily (MNT_DETACH), which a busy mount
	// does not refuse.
	UmountDetach = "umount-detach"
)

// ExitRefused is the mount mode's exit status for a call the system
// refused.
const ExitRefused = 1

// MountOps are the mount mode's operations.
func MountOps() []string { return []string{MountBindRW, MountRW, MountBindRO, Umount, UmountDetach} }

// mountPoints are the mount points the mount mode may name: the two roots
// and the writable part.
func mountPoints(work, nonce string) []string {
	ro, dev := filepath.Join(work, "conf-"+nonce[:8]+"-ro"), filepath.Join(work, "conf-"+nonce[:8]+"-dev")
	return []string{ro, dev, filepath.Join(dev, "part")}
}

// errnoText is strerror's text in the C locale for the errors a mount
// call can end with; the suite reads these words.
var errnoText = map[syscall.Errno]string{
	syscall.EPERM:   "Operation not permitted",
	syscall.EACCES:  "Permission denied",
	syscall.EBUSY:   "Device or resource busy",
	syscall.EROFS:   "Read-only file system",
	syscall.ENOENT:  "No such file or directory",
	syscall.EINVAL:  "Invalid argument",
	syscall.ENOSYS:  "Function not implemented",
	syscall.ENOTDIR: "Not a directory",
}

// ErrnoText is what the probe prints for err.
func ErrnoText(err error) string {
	var e syscall.Errno
	if errors.As(err, &e) {
		if t, ok := errnoText[e]; ok {
			return t
		}
		return fmt.Sprintf("errno %d", int(e))
	}
	return clip(err.Error())
}

// Mount checks the gate and the operation, then makes the one call. A
// gate that refuses is a *GateError and nothing is called; nil is a call
// that succeeded.
func Mount(work, nonce, op, target string) error {
	if err := Gate(work, nonce); err != nil {
		return &GateError{err}
	}
	if !slices.Contains(MountOps(), op) {
		return &usageError{fmt.Errorf("%q is not a mount operation the probe knows", op)}
	}
	if !slices.Contains(mountPoints(work, nonce), target) {
		return &usageError{fmt.Errorf("%q is not one of the suite's mount points", target)}
	}
	return mountCall(op, target)
}

type usageError struct{ error }

// MountUserNS is the operation that enters a new user and mount namespace
// (which gVisor allows an unprivileged process, and a container's seccomp
// refuses) and from inside it tries to make a read-only root writable and
// to unmount the part, then writes to a read-only canary as an
// independent witness that the root is still read-only for real. It needs
// --part and --witness besides --target, so it is not in MountOps and
// does not go through the single-call Mount.
const MountUserNS = "userns-remount"

// UserNS runs the user-namespace attempt: target is a read-only root's
// mount point, part the writable part's mount point (unmounted from
// inside the namespace), witness a read-only canary under target written
// to from inside it. held reports that the protection held (the namespace
// was refused, or every call and the witness write were refused); the
// lines say what each step came to. The suite also checks the witness on
// the host: a write that landed is a breach whatever held says.
func UserNS(work, nonce, target, part, witness string) (held bool, lines []string, err error) {
	if err := checkUserNS(work, nonce, target, part, witness); err != nil {
		return false, nil, err
	}
	return userNS(work, nonce, target, part, witness)
}

// checkUserNS is the gate and the paths' checks of the user-namespace mode.
func checkUserNS(work, nonce, target, part, witness string) error {
	if err := Gate(work, nonce); err != nil {
		return &GateError{err}
	}
	mps := mountPoints(work, nonce)
	ro, p := mps[0], mps[2]
	switch {
	case target != ro:
		return &usageError{fmt.Errorf("--target %q is not the read-only root", target)}
	case part != p:
		return &usageError{fmt.Errorf("--part %q is not the writable part", part)}
	case !strings.HasPrefix(witness, target+"/") || !filepath.IsAbs(witness) || filepath.Clean(witness) != witness:
		return &usageError{fmt.Errorf("--witness %q is not a path under the read-only root", witness)}
	}
	return nil
}

// UserNSChild is the copy of the probe userNS starts in the new
// namespace: it checks its arguments as UserNS did (the work directory,
// which must be its own, the gate, then the paths), and makes the
// attempts. It is reached only through userNSChildArg, as the first
// argument.
func UserNSChild(work string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 5 || args[0] != work {
		fmt.Fprint(stderr, usage)
		return ExitUsage
	}
	nonce, args := args[1], args[2:]
	if err := checkUserNS(work, nonce, args[0], args[1], args[2]); err != nil {
		fmt.Fprintf(stderr, "conformance-probe: %s\n", clip(err.Error()))
		return ExitUsage
	}
	userNSAttempts(args[0], args[1], args[2], stdout)
	return 0
}
