package conformance

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// scanNameRe is a canary's name: conf-<nonce8>-<role>.
var scanNameRe = regexp.MustCompile(`^conf-[0-9a-f]{8}-[a-z][a-z0-9-]{0,23}$`)

// ScanPrefix starts the body of every FindNames command: the scan is
// bounded to 60 seconds.
const ScanPrefix = "timeout 60 find / "

// FindNames searches the sandbox's whole filesystem, every mount included
// but /proc, /sys and /dev, for entries named exactly one of names (canary
// names, matched whole, no pattern), and prints each path found to
// stderr. It reads only.
func FindNames(names ...string) (Cmd, error) {
	if len(names) == 0 || len(names) > 8 {
		return Cmd{}, errors.New("conformance: a scan looks for one to eight canary names")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s-path /proc -prune -o -path /sys -prune -o -path /dev -prune", ScanPrefix)
	for _, n := range names {
		if !scanNameRe.MatchString(n) {
			return Cmd{}, fmt.Errorf("conformance: %q is not a canary's name", n)
		}
		fmt.Fprintf(&b, " -o -name '%s' -print", n)
	}
	b.WriteString(" 1>&2")
	return Cmd{body: b.String()}, nil
}
