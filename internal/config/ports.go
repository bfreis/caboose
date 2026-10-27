package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bfreis/caboose/internal/uiport"
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

// UIPort is the first of the web UI's ports (uiport) the set holds, and
// whether it holds one.
func (s PortSet) UIPort() (int, bool) {
	for _, r := range s {
		if p, ok := uiport.Covering(r.lo, r.hi); ok {
			return p, true
		}
	}
	return 0, false
}

// CheckForwardPorts refuses a forward_ports set that holds one of the web
// UI's ports: the page approves what a sandbox asks for, so a sandbox may
// never listen on its port on this machine's localhost. key names the
// setting in the error.
func CheckForwardPorts(key string, s PortSet) error {
	if p, ok := s.UIPort(); ok {
		return fmt.Errorf("%s includes port %d, which is never forwarded: it is %s; leave it out", key, p, uiport.What)
	}
	return nil
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

// PortsValue is forward_ports, s as ParsePorts reads it, as the list
// config.toml holds: a range as a string ("3000-3999"), a lone port as a
// number (5173), and "none" as no entries. It is what Edit.Set takes.
func PortsValue(s string) ([]any, error) {
	if _, err := ParsePorts(s); err != nil {
		return nil, err
	}
	s = strings.TrimSpace(s)
	list := []any{}
	if s == "none" {
		return list, nil
	}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if strings.Contains(f, "-") {
			list = append(list, f)
			continue
		}
		p, err := strconv.Atoi(f)
		if err != nil {
			return nil, err
		}
		list = append(list, int64(p))
	}
	return list, nil
}
