package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerOf is the user (LOCAL_PEERCRED) and process (LOCAL_PEERPID: the
// credentials hold no PID on macOS) at the other end of c. A PID that
// cannot be read is 0; a user that cannot be is an error.
func peerOf(c net.Conn) (peerCred, error) {
	var cred *unix.Xucred
	var err error
	pid := 0
	if cerr := rawControl(c, func(fd uintptr) {
		cred, err = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if p, perr := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID); perr == nil && p > 0 {
			pid = p
		}
	}); cerr != nil {
		return peerCred{}, cerr
	}
	if err != nil {
		return peerCred{}, err
	}
	return peerCred{UID: cred.Uid, PID: pid}, nil
}

// executableOf is the executable path of process pid: the first string of
// its kern.procargs2, after the argument count; "" when it cannot be
// read.
func executableOf(pid int) string {
	if pid <= 0 {
		return ""
	}
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(b) <= 4 {
		return ""
	}
	return boundExecutable(string(procargsPath(b)))
}
