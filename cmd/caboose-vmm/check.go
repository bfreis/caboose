package main

import (
	"os"
	"runtime"
	"time"

	"github.com/bfreis/caboose/internal/version"
	"github.com/bfreis/caboose/internal/vm"
)

// checkTimeout bounds --check: the framework answers in milliseconds, so
// anything longer is a hang to report, not to wait out.
const checkTimeout = 5 * time.Second

// runCheck prints check's report and is the exit code: 0 when a VM can
// run, 1 when not.
func runCheck() int {
	base := vm.Check{Version: version.Get().Version, OS: runtime.GOOS, Arch: runtime.GOARCH}
	done := make(chan vm.Check, 1)
	go func() { done <- check() }()
	c := base
	select {
	case r := <-done:
		c = r
		c.Version, c.OS, c.Arch = base.Version, base.OS, base.Arch
	case <-time.After(checkTimeout):
		c.Error = "Virtualization.framework did not answer in " + checkTimeout.String()
	}
	if err := c.Write(os.Stdout); err != nil {
		return 1
	}
	if !c.Usable() {
		return 1
	}
	return 0
}
