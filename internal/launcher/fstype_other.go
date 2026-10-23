//go:build !linux && !darwin

package launcher

// fsTypeName: no way to tell the filesystem apart on this platform.
func fsTypeName(string) (string, error) { return "", nil }
