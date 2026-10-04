package agent

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/linkdebug"
)

// vsockListener listens on a vsock port of the guest. Go's net package
// knows no vsock, so its connections are plain files.
type vsockListener struct{ fd int }

// vsockBuffer is each vsock connection's receive buffer in the guest.
const vsockBuffer = 4 << 20

func listenVsock(port uint32) (*vsockListener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock: %w", err)
	}
	// What the host may send ahead on a connection is the guest's receive
	// buffer, Linux's 256 KiB unless raised: at most that much per round
	// trip through the hypervisor, whatever the link's windows
	// (agentproto.AgentWindow). Each connection takes it from the
	// listener. Best effort: a kernel that refuses keeps its own.
	if !linkdebug.Get().NoGuestBuf {
		_ = unix.SetsockoptUint64(fd, unix.AF_VSOCK, unix.SO_VM_SOCKETS_BUFFER_MAX_SIZE, vsockBuffer)
		_ = unix.SetsockoptUint64(fd, unix.AF_VSOCK, unix.SO_VM_SOCKETS_BUFFER_SIZE, vsockBuffer)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock port %d: %w", port, err)
	}
	return &vsockListener{fd: fd}, nil
}

// Accept waits for the host's next connection.
func (l *vsockListener) Accept() (*os.File, error) {
	for {
		nfd, _, err := unix.Accept4(l.fd, unix.SOCK_CLOEXEC)
		if err == unix.EINTR || err == unix.ECONNABORTED {
			continue
		}
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(nfd), "vsock"), nil
	}
}

// serve runs handle on each connection l accepts, each in its own
// goroutine, until l fails.
func (l *vsockListener) serve(handle func(io.ReadWriteCloser)) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go handle(c)
	}
}
