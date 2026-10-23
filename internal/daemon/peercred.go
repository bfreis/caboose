package daemon

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// checkPeer refuses a peer whose user is not the daemon's own.
func checkPeer(uid, own uint32) error {
	if uid != own {
		return fmt.Errorf("refused a connection from uid %d: this daemon serves uid %d only", uid, own)
	}
	return nil
}

// rawControl runs f on c's file descriptor; c must be a Unix socket.
func rawControl(c net.Conn, f func(fd uintptr)) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return errors.New("not a Unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	return raw.Control(f)
}

// ownUID is the daemon's user.
func ownUID() uint32 { return uint32(syscall.Getuid()) }
