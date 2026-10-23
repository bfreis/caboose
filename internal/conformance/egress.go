package conformance

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strconv"
)

// The egress checks' host side: the suite's listeners, the throwaway
// configuration's egress keys, and the launcher's decision on Claude
// Code's hosts. A check reaches nothing but a listener the suite started,
// on an address the suite chose: a Target is made only from one.

// Scope is where a suite listener is bound.
type Scope string

const (
	// ScopeLoopback is 127.0.0.1.
	ScopeLoopback Scope = "loopback"
	// ScopeLAN is this machine's own private (LAN) address, chosen from
	// its interfaces by PickLAN.
	ScopeLAN Scope = "lan"
)

// ErrNoLAN is what EgressOps.StartListener returns for ScopeLAN on a machine
// with no private address: the checks that need one are SKIP.
var ErrNoLAN = errors.New("conformance: this machine has no private address to listen on")

// SuiteListener is a listener of the suite's own: it accepts, reads up to
// 64 bytes, replies, closes, and counts. Received is the first line each
// connection sent, in order.
type SuiteListener interface {
	Listener
	Received() []string
}

// EgressPolicy is the throwaway vm profile's egress keys: egress_allow,
// egress_deny and egress_ports. Ports nil leaves egress_ports unset (its
// default).
type EgressPolicy struct {
	Allow, Deny []string
	Ports       []int
}

// Equal reports whether p and q say the same.
func (p EgressPolicy) Equal(q EgressPolicy) bool {
	return slices.Equal(p.Allow, q.Allow) && slices.Equal(p.Deny, q.Deny) &&
		slices.Equal(p.Ports, q.Ports) && (p.Ports == nil) == (q.Ports == nil)
}

// EgressOps are the host-side calls of the egress checks. The real Host
// implements them in the launcher's conformance command; a Host that does
// not makes every egress check an ERROR, never a PASS.
//
// None of them may reach anything but the suite's own listeners, or
// change anything but the environment's own config.toml under B.
type EgressOps interface {
	// StartListener starts a suite listener at scope, on a port the kernel
	// chooses (ListenAt); ErrNoLAN when there is no LAN address. The check
	// closes it.
	StartListener(ctx context.Context, scope Scope) (SuiteListener, error)
	// EgressKeys are the profile's egress keys now.
	EgressKeys() (EgressPolicy, error)
	// SetEgress rewrites them in the environment's config.toml, and
	// returns once the running link decides by them.
	SetEgress(p EgressPolicy) error
	// LaunchDecision is the launcher's create and attach decision on a
	// copy of the configuration, under B, with the egress keys p says: its
	// refusal and message. It creates, starts and changes nothing.
	LaunchDecision(ctx context.Context, p EgressPolicy) (LaunchResult, error)
}

// EgressOps are the egress calls, inside the envelope only.
func (e Env) EgressOps() (EgressOps, error) {
	if err := e.sealed(); err != nil {
		return nil, err
	}
	ops, ok := e.host.(EgressOps)
	if !ok || ops == nil {
		return nil, errors.New("conformance: this host offers no egress calls")
	}
	return ops, nil
}

// Target is an address and port a check connects to: one of the suite's
// listeners (TargetOf), or the documentation address beside one
// (DocTarget). Nothing else makes one.
type Target struct {
	addr netip.Addr
	port int
	// name is set when the target is the loopback listener by the name
	// localhost.
	name string
}

// DocAddr is TEST-NET-1's first address (RFC 5737): routed nowhere, so no
// packet sent to it reaches a host.
var DocAddr = netip.MustParseAddr("192.0.2.1")

// listenerAddr is a suite listener's address: loopback 127.0.0.1, or a
// private address (RFC 1918 or ULA). Never a public one.
func listenerAddr(a netip.Addr) bool {
	if a == netip.MustParseAddr("127.0.0.1") {
		return true
	}
	return a.Zone() == "" && !a.Is4In6() && isPrivate(a)
}

var privateNets = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

func isPrivate(a netip.Addr) bool {
	for _, p := range privateNets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// TargetOf is the target of listener l: its address, which must be one a
// suite listener binds, and its port.
func TargetOf(l Listener) (Target, error) {
	if l == nil {
		return Target{}, errors.New("conformance: no listener")
	}
	ap, err := netip.ParseAddrPort(l.Addr())
	if err != nil || ap.Port() == 0 || !listenerAddr(ap.Addr()) {
		return Target{}, errors.New("conformance: a target is a suite listener's address")
	}
	return Target{addr: ap.Addr(), port: int(ap.Port())}, nil
}

// ByName is t by the name localhost, for the loopback listener only.
func (t Target) ByName() (Target, error) {
	if t.addr != netip.MustParseAddr("127.0.0.1") {
		return Target{}, errors.New("conformance: only the loopback listener has a name")
	}
	t.name = "localhost"
	return t, nil
}

// DocTarget is DocAddr at t's port.
func DocTarget(t Target) (Target, error) {
	if !t.valid() {
		return Target{}, errors.New("conformance: no target")
	}
	return Target{addr: DocAddr, port: t.port}, nil
}

func (t Target) valid() bool {
	return t.addr.IsValid() && t.port > 0 && t.port < 65536 && (listenerAddr(t.addr) || t.addr == DocAddr)
}

// Addr is the target's address.
func (t Target) Addr() netip.Addr { return t.addr }

// Port is the target's port.
func (t Target) Port() int { return t.port }

// Host is what a client is given: the name, or the address.
func (t Target) Host() string {
	if t.name != "" {
		return t.name
	}
	return t.addr.String()
}

func (t Target) String() string {
	return netip.AddrPortFrom(t.addr, uint16(t.port)).String()
}

// hostPort is host:port as a URL's authority writes it.
func (t Target) hostPort() string {
	h := t.Host()
	if t.name == "" && t.addr.Is6() {
		h = "[" + h + "]"
	}
	return h + ":" + strconv.Itoa(t.port)
}

// CIDR is the network around a LAN target's address that holds it and is
// wider than it: /24 for IPv4, /64 for IPv6. An egress_allow entry that
// names it is not the address itself.
func (t Target) CIDR() (string, error) {
	if !t.valid() || !isPrivate(t.addr) {
		return "", errors.New("conformance: only a LAN target has a network")
	}
	bits := 24
	if t.addr.Is6() {
		bits = 64
	}
	p, _ := t.addr.Prefix(bits)
	return p.String(), nil
}
