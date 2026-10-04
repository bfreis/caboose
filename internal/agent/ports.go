package agent

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// tcpListen is the state column's value for a listening socket in
// /proc/net/tcp and tcp6.
const tcpListen = "0A"

// parseProcNet returns the ports of the listening sockets in a
// /proc/net/tcp or tcp6 table. A line it cannot read is skipped: the table
// is the kernel's, but a parse error must never stop the watcher.
func parseProcNet(r io.Reader) []int {
	var ports []int
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		if first { // the header
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[3] != tcpListen {
			continue
		}
		_, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		p, err := strconv.ParseUint(hexPort, 16, 16)
		if err != nil || p == 0 {
			continue
		}
		ports = append(ports, int(p))
	}
	return ports
}

// listeningPorts is every TCP port something listens on in the container,
// sorted and without repeats, read from procRoot/net/tcp and tcp6.
func listeningPorts(procRoot string) []int {
	var all []int
	for _, name := range []string{"tcp", "tcp6"} {
		f, err := os.Open(filepath.Join(procRoot, "net", name))
		if err != nil {
			continue
		}
		all = append(all, parseProcNet(f)...)
		f.Close()
	}
	slices.Sort(all)
	return slices.Compact(all)
}
