package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseMemory is an amount of memory in MiB: "8G", "8GiB", "4096M",
// "4096MiB", or a number of MiB. It is what a VM profile's memory is read
// as, and what a settings proposal's is checked with.
func ParseMemory(s string) (int, error) {
	t := strings.TrimSpace(strings.ToUpper(s))
	mul := 1
	switch {
	case strings.HasSuffix(t, "GIB"), strings.HasSuffix(t, "GB"), strings.HasSuffix(t, "G"):
		mul, t = 1024, strings.TrimRight(t, "GIB")
	case strings.HasSuffix(t, "MIB"), strings.HasSuffix(t, "MB"), strings.HasSuffix(t, "M"):
		t = strings.TrimRight(t, "MIB")
	}
	n, err := strconv.Atoi(t)
	if err != nil || n < 1 || n > 1<<30 {
		return 0, fmt.Errorf("%q is not an amount like \"8G\" or \"4096M\"", s)
	}
	if n*mul < 512 {
		return 0, fmt.Errorf("%q is under 512M, too little for the sandbox", s)
	}
	return n * mul, nil
}
