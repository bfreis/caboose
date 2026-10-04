// Package tty answers `[ -t fd ]` without a dependency: a file descriptor is
// a terminal when the terminal-attributes ioctl succeeds on it. It also
// sizes one, and puts one in raw mode for a terminal in a vm guest.
package tty

import (
	"syscall"
	"unsafe"
)

// IsTerminal reports whether fd refers to a terminal.
func IsTerminal(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlGetTermios, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// Width is the terminal's width in columns, or 0 when fd is not a terminal
// or does not say.
func Width(fd uintptr) int {
	_, cols := Size(fd)
	return int(cols)
}

// Size is the terminal's rows and columns, or zeros when fd is not a
// terminal or does not say.
func Size(fd uintptr) (rows, cols uint16) {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0, 0
	}
	return ws.Row, ws.Col
}

// MakeRaw puts the terminal at fd into raw mode, as cfmakeraw(3) does, for
// a remote terminal to have every key; restore puts it back.
func MakeRaw(fd uintptr) (restore func(), err error) {
	var old syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlGetTermios, uintptr(unsafe.Pointer(&old))); errno != 0 {
		return nil, errno
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlSetTermios, uintptr(unsafe.Pointer(&raw))); errno != 0 {
		return nil, errno
	}
	return func() {
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlSetTermios, uintptr(unsafe.Pointer(&old)))
	}, nil
}
