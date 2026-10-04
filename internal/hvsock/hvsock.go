// Package hvsock is Cloud Hypervisor's hybrid vsock: a Unix socket on the
// host, vm.sock, through which the host reaches a vsock port of the guest.
// The host connects, writes "CONNECT <port>\n", and reads "OK <n>\n" once
// something in the guest listens on that port; from then on the connection
// is the guest's. Firecracker's socket speaks the same. caboose's vmm
// serves it on the Mac too, so the launcher and the link reach a guest the
// same way whatever runs it, and only vmm knows the runner.
package hvsock

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/linkdebug"
)

// maxLine bounds a handshake line, either way: "CONNECT 4294967295\n" and
// "OK 4294967295\n" are far shorter.
const maxLine = 64

// ErrRefused is a CONNECT the other end closed without an OK: nothing in
// the guest listens on the port yet, typically because it is still booting.
var ErrRefused = errors.New("hvsock: nothing listens on that port in the guest")

// Dial connects to port in the guest behind socket, and returns once the
// guest has accepted, within timeout for the whole handshake.
func Dial(socket string, port uint32, timeout time.Duration) (net.Conn, error) {
	c, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return nil, err
	}
	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		c.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		c.Close()
		return nil, err
	}
	line, err := readLine(c)
	if err != nil {
		c.Close()
		if errors.Is(err, io.EOF) {
			return nil, ErrRefused
		}
		return nil, fmt.Errorf("hvsock: port %d: %w", port, err)
	}
	if f := strings.Fields(line); len(f) != 2 || f[0] != "OK" {
		c.Close()
		return nil, fmt.Errorf("hvsock: port %d: unexpected answer %q", port, line)
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		c.Close()
		return nil, err
	}
	// A Mac's Unix sockets buffer 8 KiB unless asked: a link's every
	// frame would take several trips through the system. The system may
	// give less.
	if uc, ok := c.(*net.UnixConn); ok && !linkdebug.Get().NoSockBuf {
		_ = uc.SetReadBuffer(sockBuffer)
		_ = uc.SetWriteBuffer(sockBuffer)
	}
	return c, nil
}

// sockBuffer is what Dial asks for each way on its connection.
const sockBuffer = 1 << 20

// readLine reads up to a newline, a byte at a time: whatever follows it is
// the guest's, and must stay unread in c.
func readLine(c io.Reader) (string, error) {
	var b [1]byte
	var line []byte
	for len(line) < maxLine {
		if _, err := io.ReadFull(c, b[:]); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				err = io.EOF
			}
			return "", err
		}
		if b[0] == '\n' {
			return string(line), nil
		}
		line = append(line, b[0])
	}
	return "", errors.New("handshake line too long")
}

// Accept reads the CONNECT that opens c, a connection to vm.sock, and
// returns the port asked for; the server answers with Ready once it has
// the guest's end, or closes c.
func Accept(c net.Conn, timeout time.Duration) (uint32, error) {
	if err := c.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}
	line, err := readLine(c)
	if err != nil {
		return 0, err
	}
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		return 0, err
	}
	verb, arg, _ := strings.Cut(line, " ")
	port, err := strconv.ParseUint(arg, 10, 32)
	if verb != "CONNECT" || err != nil {
		return 0, fmt.Errorf("hvsock: bad request %q", line)
	}
	return uint32(port), nil
}

// Ready tells the host its connection is now the guest's; local is the
// host side's port number, which caboose's client ignores.
func Ready(c net.Conn, local uint32) error {
	_, err := fmt.Fprintf(c, "OK %d\n", local)
	return err
}
