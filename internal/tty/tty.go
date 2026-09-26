// Package tty answers `[ -t fd ]` without a dependency: a file descriptor is
// a terminal when the terminal-attributes ioctl succeeds on it.
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
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0
	}
	return int(ws.Col)
}
