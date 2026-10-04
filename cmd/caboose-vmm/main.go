// Command caboose-vmm owns one caboose VM: caboose-vmm DIR boots the VM of
// DIR's machine.json and serves DIR's vm.sock until the VM stops, or until
// a SIGTERM, which shuts the guest down first (internal/vm/vmm). The
// launcher starts it, detached; nobody else needs to.
//
// caboose-vmm --check says, as key=value lines on stdout (vm.Check),
// whether it can run a VM here -- the framework, the Mac's support, its
// own entitlement, the macOS and its own version -- and exits 0 only when
// it can. setup and doctor ask it.
//
// It is a binary of its own, beside the launcher, because it alone carries
// the Virtualization.framework code and its entitlement: a security tool
// that refuses it stops vm, never caboose.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/version"
	"github.com/bfreis/caboose/internal/vm"
	"github.com/bfreis/caboose/internal/vm/vmm"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("caboose-vmm", version.Get().Version)
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--check" {
		os.Exit(runCheck())
	}
	if len(os.Args) != 2 || os.Args[1] == "" || os.Args[1][0] == '-' {
		fmt.Fprintln(os.Stderr, "usage: caboose-vmm DIR (the launcher runs it; see caboose help)")
		os.Exit(2)
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "%s caboose-vmm: %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
	}
	r, err := newRunner()
	if err != nil {
		logf("%v", err)
		os.Exit(1)
	}
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := vmm.Run(ctx, vm.Dir(os.Args[1]), r, logf); err != nil {
		logf("%v", err)
		os.Exit(1)
	}
}
