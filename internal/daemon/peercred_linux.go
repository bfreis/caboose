package daemon

import (
	"net"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// peerOf is the user and process at the other end of c (SO_PEERCRED).
func peerOf(c net.Conn) (peerCred, error) {
	var cred *unix.Ucred
	var err error
	if cerr := rawControl(c, func(fd uintptr) {
		cred, err = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); cerr != nil {
		return peerCred{}, cerr
	}
	if err != nil {
		return peerCred{}, err
	}
	return peerCred{UID: cred.Uid, PID: int(cred.Pid)}, nil
}

// executableOf is the executable path of process pid, from
// /proc/PID/exe; "" when it cannot be read.
func executableOf(pid int) string {
	if pid <= 0 {
		return ""
	}
	p, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return ""
	}
	return boundExecutable(p)
}
