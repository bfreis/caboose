//go:build !darwin

package launcher

import "golang.org/x/sys/unix"

// hostMemory is this machine's memory in bytes, 0 when it cannot be told.
func hostMemory() uint64 {
	var si unix.Sysinfo_t
	if unix.Sysinfo(&si) != nil {
		return 0
	}
	return uint64(si.Totalram) * uint64(si.Unit)
}
