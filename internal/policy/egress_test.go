package policy

import (
	"net/netip"
	"strings"
	"testing"
)

func TestDenied(t *testing.T) {
	for s, want := range map[string]bool{
		"127.0.0.1": true, "127.8.9.10": true, "::1": true, "::ffff:127.0.0.1": true, "::": true, "0.0.0.0": true,
		"10.1.2.3": true, "172.16.0.1": true, "172.31.255.255": true, "192.168.1.1": true,
		"192.168.64.2": true, // vmnet's subnet
		"100.64.0.1":   true, "100.127.255.255": true, "169.254.169.254": true,
		"fe80::1": true, "fc00::1": true, "fd7a:115c:a1e0::1": true, "fec0::1": true,
		"ff02::1": true, "224.0.0.251": true, "239.255.255.250": true, "255.255.255.255": true,
		"::ffff:10.0.0.1": true, "::ffff:192.168.64.1": true, "::127.0.0.1": true,
		"64:ff9b::7f00:1": true, "2002:a00:1::": true, // 127.0.0.1 by NAT64, 10.0.0.1 by 6to4
		"::ffff:0:7f00:1": true, "::ffff:0:a00:1": true, // IPv4-translated 127.0.0.1, 10.0.0.1
		"64:ff9b:1::808:808": true, "64:ff9b:1:808:8:800::": true, // local-use NAT64, whatever it embeds

		"8.8.8.8": false, "140.82.112.3": false, "100.63.255.255": false, "100.128.0.1": false,
		"172.32.0.1": false, "192.169.0.1": false, "2606:4700:4700::1111": false,
		"::ffff:8.8.8.8": false, "64:ff9b::808:808": false, "2002:808:808::": false,
		"::ffff:0:808:808": false,
	} {
		if got := Denied(netip.MustParseAddr(s)); got != want {
			t.Errorf("Denied(%s) = %v", s, got)
		}
	}
	if !Denied(netip.MustParseAddr("fe80::1%en0")) || !Denied(netip.Addr{}) {
		t.Error("an address with a zone, or none, is allowed")
	}
}

func TestParseAllow(t *testing.T) {
	a, err := ParseAllow("git.corp.example, *.internal.example 10.20.0.0/16 192.168.1.5 ::ffff:172.16.0.0/108 FD00::/8")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"git.corp.example": true, "corp.example": false, "x.git.corp.example": false,
		"a.internal.example": true, "a.b.internal.example": true, "internal.example": false,
		"evilinternal.example": false,
	} {
		if a.Name(name) != want {
			t.Errorf("Name(%s) = %v", name, !want)
		}
	}
	for s, want := range map[string]bool{
		"10.20.3.4": true, "10.21.0.1": false, "192.168.1.5": true, "192.168.1.6": false,
		"::ffff:10.20.0.9": true, "172.16.1.1": true, "172.32.0.1": false, "fd00::1": true, "fc00::1": false,
	} {
		if a.Addr(netip.MustParseAddr(s)) != want {
			t.Errorf("Addr(%s) = %v", s, !want)
		}
	}
	for _, ok := range []string{"", "none", "  "} {
		if a, err := ParseAllow(ok); err != nil || a.Name("x") || a.Addr(netip.MustParseAddr("10.0.0.1")) {
			t.Errorf("ParseAllow(%q): %+v %v", ok, a, err)
		}
	}
	for _, bad := range []string{"10.0.0.0/33", "*", "*.", "a..b", "a b/c", "host:22", "fe80::/10%en0", "*.*.x", "ex ample!", strings.Repeat("a", 64) + ".com"} {
		if _, err := ParseAllow(bad); err == nil {
			t.Errorf("ParseAllow(%q) accepted", bad)
		}
	}
}

// denied reports whether Decide, under fallible trust, refuses to dial
// name (an address literal when "") at addrs: what the proxy's check said
// before a private address could be asked about.
func denied(t *testing.T, allow, name string, as []netip.Addr, own []netip.Addr) bool {
	t.Helper()
	p := egressPolicy(t, TrustFallible, allow, "")
	r := Request{Kind: KindEgress, Host: name, Port: 443, Addrs: as, Own: own}
	if name == "" {
		r.Host, r.Literal = as[0].String(), true
	}
	return Decide(p, r, t0).Verdict != VerdictAllow
}

// A name that resolves to a private address is refused, whatever else it
// resolves to, unless egress_allow names it or that address.
func TestEgressDeniedResolution(t *testing.T) {
	pub, priv := netip.MustParseAddr("140.82.112.3"), netip.MustParseAddr("10.0.0.7")
	cases := []struct {
		allow  string
		addrs  []netip.Addr
		denied bool
	}{
		{"", []netip.Addr{pub}, false},
		{"", []netip.Addr{priv}, true},
		{"", []netip.Addr{pub, priv}, true},
		{"", []netip.Addr{priv, pub}, true},
		{"rebind.example", []netip.Addr{pub, priv}, false},
		{"rebind.example", []netip.Addr{pub}, false},
		{"*.example", []netip.Addr{priv}, false},
		{"rebind.example 10.0.0.7", []netip.Addr{pub, priv}, false},
		{"10.0.0.0/24", []netip.Addr{pub, priv}, false},
		{"10.0.1.0/24", []netip.Addr{pub, priv}, true},
		{"other.example", []netip.Addr{priv}, true},
	}
	for _, c := range cases {
		if denied := denied(t, c.allow, "rebind.example", c.addrs, nil); denied != c.denied {
			t.Errorf("allow %q, %v: denied %v", c.allow, c.addrs, denied)
		}
	}
}

// A name egress_allow names still never reaches loopback, unspecified or
// link-local addresses, nor this machine's own, in any form: only an
// address or CIDR within that range lets one through.
func TestEgressDeniedLocal(t *testing.T) {
	own := []netip.Addr{netip.MustParseAddr("2001:db8:1::5"), netip.MustParseAddr("203.0.113.9"), netip.MustParseAddr("10.1.2.3")}
	cases := []struct {
		allow, addr string
		denied      bool
	}{
		{"*.nip.io", "127.0.0.1", true},
		{"*.nip.io", "0.0.0.0", true},
		{"*.nip.io", "::", true},
		{"*.nip.io", "::1", true},
		{"*.nip.io", "169.254.169.254", true},
		{"*.nip.io", "fe80::1", true},
		{"*.nip.io", "::ffff:127.0.0.1", true},
		{"*.nip.io", "64:ff9b::7f00:1", true},
		{"*.nip.io", "::ffff:0:7f00:1", true},
		{"*.nip.io", "64:ff9b:1::7f00:1", true},
		{"*.nip.io", "2002:7f00:1::", true},
		{"*.nip.io", "10.0.0.7", false}, // private, and allowed by name
		{"10.0.0.7", "10.0.0.7", false},
		{"*.nip.io", "140.82.112.3", false},
		{"0.0.0.0/0", "127.0.0.1", true},    // wider than loopback
		{"127.0.0.0/8", "127.0.0.1", false}, // loopback, by name
		{"127.0.0.1", "127.0.0.1", false},
		{"127.0.0.1", "64:ff9b::7f00:1", false},
		{"169.254.169.254", "169.254.169.254", false},
		{"fe80::/10", "fe80::1", false},
		// This machine's own addresses.
		{"", "2001:db8:1::5", true},
		{"", "203.0.113.9", true},
		{"", "64:ff9b::cb00:7109", true}, // 203.0.113.9 by NAT64
		{"*.nip.io", "2001:db8:1::5", true},
		{"2001:db8::/32", "2001:db8:1::5", true},
		{"10.0.0.0/8", "10.1.2.3", true},
		{"10.0.0.0/8", "10.1.2.4", false},
		{"2001:db8:1::5", "2001:db8:1::5", false},
		{"203.0.113.9", "203.0.113.9", false},
		{"", "2001:db8:1::6", false},
	}
	for _, c := range cases {
		if denied := denied(t, c.allow, "x.nip.io", []netip.Addr{netip.MustParseAddr(c.addr)}, own); denied != c.denied {
			t.Errorf("allow %q, %s: denied %v", c.allow, c.addr, denied)
		}
	}
	// A literal is never allowed by a pattern, whatever its digits.
	if !denied(t, "*.0.7", "", []netip.Addr{netip.MustParseAddr("10.0.0.7")}, nil) {
		t.Error("an address literal allowed by a pattern")
	}
}
