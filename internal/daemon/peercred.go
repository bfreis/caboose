package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/bfreis/caboose/internal/daemonproto"
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

// peerCred is what the socket says of the process at the other end of a
// connection: its user, and its process ID (0 when the system does not
// say).
type peerCred struct {
	UID uint32
	PID int
}

// boundExecutable is an executable path as the daemon keeps it: made
// printable and cut to daemonproto.MaxExecutable.
func boundExecutable(p string) string { return daemonproto.Text(p, daemonproto.MaxExecutable) }

// ownUID is the daemon's user.
func ownUID() uint32 { return uint32(syscall.Getuid()) }

// procargsPath is the executable path in a macOS kern.procargs2 buffer:
// after the 4-byte argument count, up to the first NUL.
func procargsPath(b []byte) []byte {
	if len(b) <= 4 {
		return nil
	}
	b = b[4:]
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return b
}
