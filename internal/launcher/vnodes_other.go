//go:build !darwin

package launcher

// sysMaxVnodes has nothing to read off a Mac.
func sysMaxVnodes() (uint32, error) { return 0, errNoVnodes }
