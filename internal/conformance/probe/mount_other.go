//go:build !linux

package probe

import (
	"io"
	"syscall"
)

// mountCall makes no call off Linux: the probe runs only in a sandbox.
func mountCall(string, string) error { return syscall.ENOSYS }

// userNS makes no call off Linux. It is a variable so that analysis of a
// build off Linux does not take UserNS for a function that always fails.
var userNS = func(string, string, string, string, string) (bool, []string, error) {
	return false, nil, syscall.ENOSYS
}

// userNSChildArg is never handed out off Linux.
const userNSChildArg = "--userns-child"

// nsRefused is never asked off Linux.
func nsRefused(error) bool { return false }

// userNSAttempts makes no call off Linux.
func userNSAttempts(_, _, _ string, _ io.Writer) {}
