package launcher

import "golang.org/x/sys/unix"

// hostMemory is this machine's memory in bytes, 0 when it cannot be told.
func hostMemory() uint64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return n
}
