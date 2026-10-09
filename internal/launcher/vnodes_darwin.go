package launcher

import "golang.org/x/sys/unix"

// sysMaxVnodes is the Mac's kern.maxvnodes.
func sysMaxVnodes() (uint32, error) { return unix.SysctlUint32("kern.maxvnodes") }
