// Command initramfs writes the kernel smoke test's initramfs to stdout:
// the init named on the command line, as /init, by vm.WriteInitramfs,
// which writes the launcher's own. make vm-kernel-smoke runs it.
package main

import (
	"fmt"
	"os"

	"github.com/bfreis/caboose/internal/vm"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: initramfs INIT > initramfs.cpio")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err == nil {
		err = vm.WriteInitramfs(os.Stdout, data)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "initramfs:", err)
		os.Exit(1)
	}
}
