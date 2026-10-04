//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package tty

import "syscall"

const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)
