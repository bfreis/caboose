package vm

import (
	"bufio"
	"fmt"
	"io"

	"github.com/bfreis/caboose/internal/linkdebug"
)

// WriteInitramfs writes the guest's initramfs to w: a newc cpio holding
// /dev, /dev/console and agent, caboose-agent for the guest's
// architecture, as /init. The launcher writes it from the agent it
// embeds, so the init always matches the launcher that boots it.
func WriteInitramfs(w io.Writer, agent []byte) error {
	bw := bufio.NewWriter(w)
	ino := uint32(1)
	add := func(name string, mode, rmaj, rmin uint32, data []byte) {
		// newc: a 110-byte header, the name and a NUL padded to 4, the
		// data padded to 4.
		fmt.Fprintf(bw, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
			ino, mode, 0, 0, 1, 0, len(data), 0, 0, rmaj, rmin, len(name)+1, 0)
		ino++
		bw.WriteString(name + "\x00")
		pad(bw, 110+len(name)+1)
		bw.Write(data)
		pad(bw, len(data))
	}
	add("dev", 0o040755, 0, 0, nil)
	add("dev/console", 0o020600, 5, 1, nil)
	add("init", 0o100755, 0, 0, agent)
	add("TRAILER!!!", 0, 0, 0, nil)
	return bw.Flush()
}

func pad(w *bufio.Writer, n int) {
	if r := n % 4; r != 0 {
		w.Write(make([]byte, 4-r))
	}
}

// DefaultCmdline is the kernel's command line when the Machine names
// none: the console on hvc0, a panic that powers off rather than hangs,
// and, with a network, the kernel's own DHCP (the agent writes
// resolv.conf from what it found). linkdebug's knobs, when set, go on it
// for the guest's agent.
func DefaultCmdline(network string) string {
	s := "console=hvc0 panic=-1 quiet loglevel=4"
	if network == "nat" {
		s += " ip=dhcp"
	}
	return s + linkdebug.Cmdline()
}
