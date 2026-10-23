package policy

// What the outbound proxy refuses by default, and egress_allow and
// egress_deny: pure functions of an address or a name. A name that
// resolves to a private address is not to be guessed about; loopback,
// unspecified and link-local addresses, and this machine's own, are
// refused even for a name egress_allow names, since a pattern such as
// *.nip.io would otherwise reach this machine's loopback. Only an
// egress_allow address or CIDR inside such a range lets one through.

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/agentproto"
)

// VMNetSubnet is macOS's vmnet shared network as it comes
// (Shared_Net_Address, 192.168.64.1 with a /24): the Mac's side of a vm
// guest and every other VM on it. The launcher cannot read what a Mac set
// it to; any address there is private anyway, and refused as RFC 1918 is.
var VMNetSubnet = netip.MustParsePrefix("192.168.64.0/24")

// deniedNets are the addresses the proxy refuses unless egress_allow names
// them: what is not the public internet as this machine routes it.
var deniedNets = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",      // this network
		"10.0.0.0/8",     // RFC 1918
		"100.64.0.0/10",  // shared address space: CGNAT, tailnet peers
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local
		"172.16.0.0/12",  // RFC 1918
		"192.168.0.0/16", // RFC 1918, vmnet's subnet among them
		"224.0.0.0/4",    // multicast
		"240.0.0.0/4",    // reserved, broadcast
		"::/96",          // unspecified, loopback, IPv4-compatible
		"64:ff9b:1::/48", // NAT64 for local use (RFC 8215)
		"fc00::/7",       // ULA
		"fe80::/10",      // link-local
		"fec0::/10",      // site-local, deprecated
		"ff00::/8",       // multicast
	} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return append(ps, VMNetSubnet)
}()

// localNets are the ranges the proxy refuses even for a name egress_allow
// names: what reaches this machine itself, or only its own links.
var localNets = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",      // this network, 0.0.0.0 among it
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local
		"::/96",          // unspecified, loopback, IPv4-compatible
		"fe80::/10",      // link-local
	} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return ps
}()

// Denied reports whether a is an address the proxy refuses by default:
// loopback, private, link-local, tailnet, the vmnet subnet, multicast and
// the like, in any of its IPv6 forms (IPv4-mapped, IPv4-translated, NAT64,
// 6to4).
func Denied(a netip.Addr) bool {
	if !a.IsValid() || a.Zone() != "" {
		return true
	}
	a = a.Unmap()
	for _, v4 := range embeddedV4(a) {
		if Denied(v4) {
			return true
		}
	}
	for _, p := range deniedNets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// localRange is the range of localNets that holds a, or an IPv4 address
// it stands for, and which of the two it holds; or that address alone,
// when it is one of own.
func localRange(a netip.Addr, own []netip.Addr) (netip.Prefix, netip.Addr, bool) {
	a = a.Unmap().WithZone("")
	for _, b := range append([]netip.Addr{a}, embeddedV4(a)...) {
		for _, p := range localNets {
			if p.Contains(b) {
				return p, b, true
			}
		}
		if slices.Contains(own, b) {
			return netip.PrefixFrom(b, b.BitLen()), b, true
		}
	}
	return netip.Prefix{}, netip.Addr{}, false
}

var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	translated = netip.MustParsePrefix("::ffff:0:0:0/96")
	sixTo4     = netip.MustParsePrefix("2002::/16")
)

// embeddedV4 is the IPv4 addresses an IPv6 one may stand for: by NAT64's
// well-known prefix, SIIT's IPv4-translated form or 6to4, one; by NAT64's
// local-use prefix, which a network may use at any of RFC 6052's lengths,
// both of the likely ones, /96's and /48's. A network-specific NAT64
// prefix (RFC 7050) is not known here: its addresses are taken as the
// IPv6 they are.
func embeddedV4(a netip.Addr) []netip.Addr {
	if !a.Is6() || a.Is4In6() {
		return nil
	}
	b := a.As16()
	switch {
	case nat64.Contains(a), translated.Contains(a):
		return []netip.Addr{netip.AddrFrom4([4]byte(b[12:16]))}
	case nat64Local.Contains(a):
		return []netip.Addr{
			netip.AddrFrom4([4]byte(b[12:16])),
			netip.AddrFrom4([4]byte{b[6], b[7], b[9], b[10]}),
		}
	case sixTo4.Contains(a):
		return []netip.Addr{netip.AddrFrom4([4]byte(b[2:6]))}
	}
	return nil
}

// ValidName reports whether s, in lower case, is a DNS name, as a host
// name the outbound proxy may be asked for must be.
func ValidName(s string) bool { return validName(s) }

// validName reports whether s, in lower case, is a DNS name: dot-separated
// labels of letters, digits, '-' and '_', each 1 to 63 bytes.
func validName(s string) bool {
	if s == "" || len(s) > agentproto.MaxEgressHost {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if l == "" || len(l) > 63 {
			return false
		}
		for _, c := range []byte(l) {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// Allow is egress_allow, or egress_deny: names, *.suffix patterns and
// CIDRs (or single addresses) the proxy may reach although they are
// private, or may not reach at all.
type Allow struct {
	names    []string
	suffixes []string // ".corp.example", for "*.corp.example"
	nets     []netip.Prefix
}

// ParseAllow reads egress_allow: entries separated by spaces or commas,
// each a host name (git.corp.example), a pattern of every name under one
// (*.corp.example, which does not match corp.example itself), a CIDR
// (10.20.0.0/16) or an address; "" or "none" allows nothing.
func ParseAllow(s string) (Allow, error) { return ParseAllowOf("egress_allow", s) }

// ParseAllowList reads a list of entries as ParseAllow reads one string,
// each entry a single one; key names the setting in errors.
func ParseAllowList(key string, entries []string) (Allow, error) {
	for _, e := range entries {
		if e == "" || e == "none" || strings.ContainsAny(e, ", \t") {
			return Allow{}, fmt.Errorf("%s: %q is not a name, *.name, CIDR or address", key, e)
		}
	}
	return ParseAllowOf(key, strings.Join(entries, " "))
}

// ParseAllowOf is ParseAllow for the setting key, which errors name.
func ParseAllowOf(key, s string) (Allow, error) {
	var a Allow
	s = strings.TrimSpace(s)
	if s == "none" {
		return a, nil
	}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if strings.Contains(f, "/") {
			p, err := netip.ParsePrefix(f)
			if err != nil || p.Addr().Zone() != "" {
				return Allow{}, fmt.Errorf("%s: %q is not a CIDR", key, f)
			}
			a.nets = append(a.nets, unmapPrefix(p).Masked())
			continue
		}
		if ip, err := netip.ParseAddr(f); err == nil && ip.Zone() == "" {
			ip = ip.Unmap()
			a.nets = append(a.nets, netip.PrefixFrom(ip, ip.BitLen()))
			continue
		}
		name := strings.TrimSuffix(strings.ToLower(f), ".")
		if rest, ok := strings.CutPrefix(name, "*."); ok {
			if !validName(rest) {
				return Allow{}, fmt.Errorf("%s: %q is not a name, *.name, CIDR or address", key, f)
			}
			a.suffixes = append(a.suffixes, "."+rest)
			continue
		}
		if !validName(name) {
			return Allow{}, fmt.Errorf("%s: %q is not a name, *.name, CIDR or address", key, f)
		}
		a.names = append(a.names, name)
	}
	return a, nil
}

// unmapPrefix is p with an IPv4-mapped address made IPv4.
func unmapPrefix(p netip.Prefix) netip.Prefix {
	if a := p.Addr(); a.Is4In6() {
		bits := p.Bits() - 96
		if bits < 0 {
			bits = 0
		}
		return netip.PrefixFrom(a.Unmap(), bits)
	}
	return p
}

// Name reports whether name (lower case, no final dot) is allowed by name.
func (a Allow) Name(name string) bool {
	if slices.Contains(a.names, name) {
		return true
	}
	for _, s := range a.suffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// Within reports whether ip is in one of the allowed CIDRs that lies
// inside r: what lets a local address (localRange) through.
func (a Allow) Within(ip netip.Addr, r netip.Prefix) bool {
	ip = ip.Unmap()
	for _, p := range a.nets {
		if p.Contains(ip) && p.Bits() >= r.Bits() && r.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// Addr reports whether ip is in one of the allowed CIDRs.
func (a Allow) Addr(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range a.nets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
