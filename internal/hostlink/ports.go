package hostlink

import (
	"fmt"
	"strconv"
	"strings"
)

// PortSet is a set of ports: which of the sandbox's may be forwarded
// (config.toml's forward_ports, parsed), or which it may reach through the
// outbound proxy (egress_ports).
type PortSet []portRange

type portRange struct{ lo, hi int }

// Has reports whether p is in the set.
func (s PortSet) Has(p int) bool {
	for _, r := range s {
		if p >= r.lo && p <= r.hi {
			return true
		}
	}
	return false
}

// ParsePorts reads forward_ports: ports and ranges (3000, 8000-8999)
// separated by spaces or commas, or "none" for no forwarding at all.
func ParsePorts(s string) (PortSet, error) { return ParsePortsOf("forward_ports", s) }

// ParsePortsOf reads a setting of ParsePorts' form, key naming it in errors.
func ParsePortsOf(key, s string) (PortSet, error) {
	s = strings.TrimSpace(s)
	if s == "none" {
		return PortSet{}, nil
	}
	var set PortSet
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		lo, hi, isRange := strings.Cut(f, "-")
		a, err := parsePort(lo)
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %v", key, f, err)
		}
		b := a
		if isRange {
			if b, err = parsePort(hi); err != nil {
				return nil, fmt.Errorf("%s: %q: %v", key, f, err)
			}
			if b < a {
				return nil, fmt.Errorf("%s: %q ends before it starts", key, f)
			}
		}
		set = append(set, portRange{a, b})
	}
	if len(set) == 0 {
		return nil, fmt.Errorf(`%s: no ports (use "none" for none)`, key)
	}
	return set, nil
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("%q is not a port, 1 to 65535", s)
	}
	return p, nil
}
