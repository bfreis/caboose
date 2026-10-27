package policy

import (
	"fmt"
	"testing"
)

// The web UI's ports on any local address are denied before any allow:
// egress_allow naming 127.0.0.1, a live grant holding the address, the
// port in egress_ports.
func TestDecideEgressUIPort(t *testing.T) {
	uiPolicy := func(allow string) Policy {
		p := egressPolicy(t, TrustFallible, allow, "")
		p.Egress.Ports = mustPorts(t, "443 7000-7999")
		return p
	}
	withAddrGrant := func(p Policy, key string, as ...string) Policy {
		g := live(KindEgress, key, "g5")
		g.Addrs = addrs(as...)
		p.Grants = []Grant{g}
		return p
	}
	own := egressReq("me.example", 7172, "203.0.113.9")
	own.Own = addrs("203.0.113.9")
	runCases(t, []decideCase{
		{"loopback literal", uiPolicy("127.0.0.1"), egressReq("127.0.0.1", 7171, "127.0.0.1"), VerdictDeny, "deny:ui_port"},
		{"loopback, granted", withAddrGrant(uiPolicy("127.0.0.0/8 0.0.0.0/0"), "egress:127.0.0.1", "127.0.0.1"),
			egressReq("127.0.0.1", 7171, "127.0.0.1"), VerdictDeny, "deny:ui_port"},
		{"loopback by name", uiPolicy("x.nip.io 127.0.0.1"), egressReq("x.nip.io", 7172, "127.0.0.1"), VerdictDeny, "deny:ui_port"},
		{"v6 loopback", uiPolicy("::1"), egressReq("::1", 7171, "::1"), VerdictDeny, "deny:ui_port"},
		{"mapped loopback", uiPolicy("::/0"), egressReq("x.example", 7171, "::ffff:127.0.0.1"), VerdictDeny, "deny:ui_port"},
		{"nat64 loopback", uiPolicy("::/0 0.0.0.0/0"), egressReq("x.example", 7171, "64:ff9b::7f00:1"), VerdictDeny, "deny:ui_port"},
		{"unspecified", uiPolicy("0.0.0.0/0"), egressReq("0.0.0.0", 7171, "0.0.0.0"), VerdictDeny, "deny:ui_port"},
		{"own address", uiPolicy("203.0.113.9 me.example"), own, VerdictDeny, "deny:ui_port"},
		{"one local among public", uiPolicy("x.example"), egressReq("x.example", 7171, "140.82.112.3", "127.0.0.1"), VerdictDeny, "deny:ui_port"},
		{"hostile", func() Policy { p := uiPolicy("127.0.0.1"); p.Trust = TrustHostile; return p }(),
			egressReq("127.0.0.1", 7171, "127.0.0.1"), VerdictDeny, "deny:ui_port"},
		// Another port on loopback, or the port on a public address, is decided as ever.
		{"loopback, other port", uiPolicy("127.0.0.1"), egressReq("127.0.0.1", 7170, "127.0.0.1"), VerdictAllow, "allow:egress_allow"},
		{"public address", uiPolicy(""), egressReq("x.example", 7171, "140.82.112.3"), VerdictAllow, "default:fallible-public"},
	})
}

// The web UI's ports are never forwarded for a port request: not when
// forward_ports holds them, not with a grant, not once the host checked.
func TestDecidePortUIPort(t *testing.T) {
	for _, port := range []int{7171, 7172} {
		for _, checked := range []bool{false, true} {
			p := portPolicy(t, TrustFallible, "1-65535", live(KindPort, fmt.Sprintf("port:%d", port), "g1"))
			runCases(t, []decideCase{{fmt.Sprintf("%d checked=%v", port, checked), p, portReq(port, checked, false), VerdictDeny, "deny:ui_port"}})
		}
	}
}
