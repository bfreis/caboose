package saferm

import "testing"

func setProc(t *testing.T, p string) {
	old := procSelfFd
	procSelfFd = p
	t.Cleanup(func() { procSelfFd = old })
}
