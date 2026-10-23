package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID is the user of the process at the other end of c (LOCAL_PEERCRED).
func peerUID(c net.Conn) (uint32, error) {
	var cred *unix.Xucred
	var err error
	if cerr := rawControl(c, func(fd uintptr) {
		cred, err = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); cerr != nil {
		return 0, cerr
	}
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
