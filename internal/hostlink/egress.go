package hostlink

// The outbound proxy. Under vm, the guest's traffic leaves through vmnet's
// NAT, which reaches none of the routes a VPN gives this machine; so the
// agent serves a proxy in the guest, and each of its connections is a
// request (OpConnect) that this machine dials, with its own resolver and
// routes, as Docker Desktop and OrbStack do for a container.
//
// The agent sends a name and a port, never an address it chose. The host
// checks the port against egress_ports, resolves the name itself and
// checks every address it got: if any is loopback, private, link-local,
// tailnet, the vmnet subnet or multicast, and egress_allow names neither
// the name nor that address, the whole request is refused -- a name that
// answers with a public address and a private one is a rebinding trick or
// a misconfiguration, and either way not one to guess about. Loopback,
// unspecified and link-local addresses, and this machine's own (its
// interfaces'), are refused even for a name egress_allow names: a pattern
// such as *.nip.io would otherwise reach this machine's loopback. Only an
// egress_allow address or CIDR inside such a range lets one through. Only
// the addresses checked are dialled, as addresses: nothing is resolved
// twice.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

// The proxy's limits.
const (
	// maxEgress is the most proxied connections open at once (the
	// session's MaxStreams is shared with the forwards).
	maxEgress = 128
	// egressRate and egressBurst bound how fast new ones open, refused
	// ones included.
	egressRate  = 20 // a second
	egressBurst = 100
	// egressResolveTimeout and egressDialTimeout bound a lookup and the
	// connect that follows it, which gives each address an equal share of
	// what is left, but no less than egressMinShare.
	egressResolveTimeout = 10 * time.Second
	egressDialTimeout    = 10 * time.Second
	egressMinShare       = 2 * time.Second
	// egressOwnEvery is how long this machine's own addresses, read from
	// its interfaces, are trusted before they are read again.
	egressOwnEvery = 5 * time.Second
	// egressLogMax is the most egress lines link.log gets in
	// egressLogEvery; the rest are counted, and the count logged once the
	// period is over.
	egressLogMax   = 10
	egressLogEvery = time.Minute
	// egressIdle closes a connection nothing has crossed, either way, in
	// that long.
	egressIdle = 15 * time.Minute
	// egressCountEvery is how often link.log gets the count of
	// connections by host:port, and maxEgressCounts how many targets it
	// counts apart.
	egressCountEvery = time.Hour
	maxEgressCounts  = 256
)

// Resolver looks names up; *net.Resolver is one.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Egress is the outbound proxy's settings: config.toml's egress_ports and
// egress_allow, and how to resolve and dial, which tests fake.
type Egress struct {
	// Listen is where, in the sandbox, the agent is to serve the proxy;
	// "" is agentproto.EgressListen.
	Listen string
	Ports  PortSet
	Allow  Allow
	// Resolver is nil for this machine's own.
	Resolver Resolver
	// Dial connects to an address (never a name); nil is a net.Dialer
	// with egressDialTimeout.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Idle is egressIdle when 0.
	Idle time.Duration
	// Own lists this machine's own addresses; nil is its interfaces'
	// (net.InterfaceAddrs).
	Own func() ([]netip.Addr, error)

	// dialTimeout and minShare are egressDialTimeout and egressMinShare
	// when 0; tests shorten them.
	dialTimeout, minShare time.Duration
}

func (e *Egress) listen() string {
	if e.Listen == "" {
		return agentproto.EgressListen
	}
	return e.Listen
}

// egressState is a Host's count of the proxy's connections.
type egressState struct {
	open    int
	tokens  float64
	last    time.Time
	limited bool // the last request was refused for a limit: log once
	counts  map[string]int
	others  int

	// own is this machine's addresses, as read at ownAt.
	own   []netip.Addr
	ownAt time.Time

	// The log's limit: lines logged since logFrom, and those dropped.
	logFrom    time.Time
	logged     int
	suppressed int
}

// egressError is a refused OpConnect: Reason for the agent, the message
// for a person.
type egressError struct{ reason, msg string }

func (e *egressError) Error() string { return e.msg }

func refuse(reason, format string, args ...any) error {
	return &egressError{reason: reason, msg: fmt.Sprintf(format, args...)}
}

// egressReason is err's Reason, "" for an error of no Egress* code.
func egressReason(err error) string {
	var e *egressError
	if errors.As(err, &e) {
		return e.reason
	}
	return ""
}

// connect serves OpConnect id, to host and port: checks it, dials it and
// opens a stream for it, or says why not. Refusals and failed dials are
// logged, by target alone.
func (h *Host) connect(id uint64, host string, port int) error {
	e := h.cfg.Egress
	if e == nil {
		return refuse(agentproto.EgressOff, "the host offers no outbound proxy (egress_proxy in config.toml on the host)")
	}
	// Every request costs a token, refused ones too: the bucket bounds
	// what reaches the log as well as what is dialled.
	if err := h.takeEgress(); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			h.releaseEgress()
		}
	}()
	name, addr, err := checkEgressHost(host)
	if err == nil && !agentproto.ValidPort(port) {
		err = refuse(agentproto.EgressBad, "%d is not a port", port)
	}
	if err != nil {
		h.egressLogf("egress %s: refused: %v", clip(printableHost(host), 80), err)
		return err
	}
	target := net.JoinHostPort(name, strconv.Itoa(port))
	if !e.Ports.Has(port) {
		err := refuse(agentproto.EgressPort, "%s: port %d is not in egress_ports (config.toml on the host)", target, port)
		h.egressLogf("egress %s: refused: not in egress_ports", target)
		return err
	}
	addrs := []netip.Addr{addr}
	if !addr.IsValid() {
		if addrs, err = e.resolve(name); err != nil {
			h.egressLogf("egress %s: %v", target, err)
			return err
		}
	}
	byName := name
	if addr.IsValid() {
		byName = "" // a literal is allowed by address, never by a pattern
	}
	if bad, denied := e.denied(byName, addrs, h.ownAddrs()); denied {
		err := refuse(agentproto.EgressDenied, "%s: %s is a loopback, private or local address, or one of the host's own, which egress_allow (config.toml on the host) does not name", target, bad)
		h.egressLogf("egress %s: refused: resolves to %s", target, bad)
		return err
	}
	conn, err := e.dial(addrs, port)
	if err != nil {
		h.egressLogf("egress %s: %v", target, err)
		return refuse(agentproto.EgressDial, "%s: %v", target, err)
	}
	hdr, _ := json.Marshal(agentproto.StreamHeader{Request: id})
	st, err := h.sess.Open(hdr)
	if err != nil {
		conn.Close()
		return err
	}
	ok = true
	h.countEgress(target)
	idle := e.Idle
	if idle <= 0 {
		idle = egressIdle
	}
	go func() {
		defer h.releaseEgress()
		spliceIdle(st, halfCloser(conn), idle)
	}()
	return nil
}

// takeEgress takes a token from the proxy's bucket and a slot among its
// open connections, or says which limit holds.
func (h *Host) takeEgress() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := &h.egress
	now := time.Now()
	if g.last.IsZero() {
		g.tokens = egressBurst
	} else {
		g.tokens = min(egressBurst, g.tokens+now.Sub(g.last).Seconds()*egressRate)
	}
	g.last = now
	var err error
	switch {
	case g.tokens < 1:
		err = refuse(agentproto.EgressLimit, "too many connections opened; try again shortly")
	case g.open >= maxEgress:
		err = refuse(agentproto.EgressLimit, "already %d connections open through the host", maxEgress)
	}
	if err != nil {
		first := !g.limited
		g.limited = true
		if first {
			h.egressLogLocked(now, "egress: refused: %v", err)
		}
		return err
	}
	g.limited = false
	g.tokens--
	g.open++
	return nil
}

func (h *Host) releaseEgress() {
	h.mu.Lock()
	h.egress.open--
	h.mu.Unlock()
}

// egressLogf logs one of the proxy's refusals or failures, at most
// egressLogMax a period: the guest decides how often they happen, and
// link.log is this machine's disk.
func (h *Host) egressLogf(format string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.egressLogLocked(time.Now(), format, args...)
}

// egressLogLocked is egressLogf, under h.mu.
func (h *Host) egressLogLocked(now time.Time, format string, args ...any) {
	g := &h.egress
	h.flushEgressLogLocked(now)
	if g.logged >= egressLogMax {
		g.suppressed++
		return
	}
	g.logged++
	h.cfg.Log.Printf(format, args...)
}

// flushEgressLogLocked starts a new period once the last is over, saying
// how many lines it dropped. Under h.mu.
func (h *Host) flushEgressLogLocked(now time.Time) {
	g := &h.egress
	if !g.logFrom.IsZero() && now.Sub(g.logFrom) < egressLogEvery {
		return
	}
	if g.suppressed > 0 {
		h.cfg.Log.Printf("egress: ... and %d more refused or failed connections not logged (at most %d lines a minute)", g.suppressed, egressLogMax)
	}
	g.logFrom, g.logged, g.suppressed = now, 0, 0
}

// ownAddrs is this machine's own addresses, read again once
// egressOwnEvery old; nil when they cannot be read.
func (h *Host) ownAddrs() []netip.Addr {
	h.mu.Lock()
	g := &h.egress
	if !g.ownAt.IsZero() && time.Since(g.ownAt) < egressOwnEvery {
		own := g.own
		h.mu.Unlock()
		return own
	}
	h.mu.Unlock()
	read := h.cfg.Egress.Own
	if read == nil {
		read = interfaceAddrs
	}
	got, err := read()
	if err != nil {
		h.egressLogf("egress: cannot list this machine's addresses: %v", err)
	}
	own := make([]netip.Addr, 0, len(got))
	for _, a := range got {
		own = append(own, a.Unmap().WithZone(""))
	}
	h.mu.Lock()
	g.own, g.ownAt = own, time.Now()
	h.mu.Unlock()
	return own
}

// interfaceAddrs is the addresses of this machine's interfaces.
func interfaceAddrs() ([]netip.Addr, error) {
	ifas, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var own []netip.Addr
	for _, ifa := range ifas {
		if n, ok := ifa.(*net.IPNet); ok {
			if a, ok := netip.AddrFromSlice(n.IP); ok {
				own = append(own, a.Unmap())
			}
		}
	}
	return own, nil
}

// countEgress counts a connection to target, for the hourly line in
// link.log.
func (h *Host) countEgress(target string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := &h.egress
	if g.counts == nil {
		g.counts = map[string]int{}
	}
	if _, ok := g.counts[target]; !ok && len(g.counts) >= maxEgressCounts {
		g.others++
		return
	}
	g.counts[target]++
}

// logEgressCounts writes the counts since the last time, if any, and
// starts over.
func (h *Host) logEgressCounts() {
	h.mu.Lock()
	g := &h.egress
	counts, others := g.counts, g.others
	g.counts, g.others = nil, 0
	h.mu.Unlock()
	if len(counts) == 0 && others == 0 {
		return
	}
	var parts []string
	for _, t := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s %d", t, counts[t]))
	}
	if others > 0 {
		parts = append(parts, fmt.Sprintf("others %d", others))
	}
	h.cfg.Log.Printf("egress, connections since the last count: %s", strings.Join(parts, ", "))
}

// countEgressEvery logs the counts every egressCountEvery until the
// session ends, and once more then.
func (h *Host) countEgressEvery() {
	t := time.NewTicker(egressCountEvery)
	defer t.Stop()
	flush := time.NewTicker(egressLogEvery)
	defer flush.Stop()
	for {
		select {
		case <-h.sess.Done():
			h.mu.Lock()
			h.flushEgressLogLocked(time.Now().Add(egressLogEvery))
			h.mu.Unlock()
			h.logEgressCounts()
			return
		case <-t.C:
			h.logEgressCounts()
		case now := <-flush.C:
			h.mu.Lock()
			h.flushEgressLogLocked(now)
			h.mu.Unlock()
		}
	}
}

// resolve looks name up with this machine's resolver.
func (e *Egress) resolve(name string) ([]netip.Addr, error) {
	r := e.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	ctx, cancel := context.WithTimeout(context.Background(), egressResolveTimeout)
	defer cancel()
	addrs, err := r.LookupNetIP(ctx, "ip", name)
	if err != nil {
		return nil, refuse(agentproto.EgressResolve, "cannot resolve %s on the host: %v", name, err)
	}
	if len(addrs) == 0 {
		return nil, refuse(agentproto.EgressResolve, "%s has no address on the host", name)
	}
	// A copy: the slice is the resolver's, which may hand it to others.
	out := make([]netip.Addr, len(addrs))
	for i, a := range addrs {
		out[i] = a.Unmap()
	}
	return out, nil
}

// denied is the first of addrs the proxy refuses to dial. An address
// Denied refuses passes when egress_allow names name ("" for none, as for
// an address literal) or the address; but a loopback, unspecified or
// link-local one, or one of own, this machine's, passes only when an
// egress_allow address or CIDR within that range (own's: that very
// address) holds it.
func (e *Egress) denied(name string, addrs []netip.Addr, own []netip.Addr) (netip.Addr, bool) {
	byName := name != "" && e.Allow.Name(name)
	for _, a := range addrs {
		if r, b, ok := localRange(a, own); ok {
			if !e.Allow.Within(b, r) {
				return a, true
			}
			continue
		}
		if Denied(a) && !byName && !e.Allow.Addr(a) {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// dial connects to the first of addrs that answers, within
// egressDialTimeout in all: each address gets an equal share of what is
// left (no less than egressMinShare), so one that never answers leaves
// time for the next.
func (e *Egress) dial(addrs []netip.Addr, port int) (net.Conn, error) {
	total, minShare := e.dialTimeout, e.minShare
	if total <= 0 {
		total = egressDialTimeout
	}
	if minShare <= 0 {
		minShare = egressMinShare
	}
	dial := e.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var err error
	for i, a := range addrs {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		share := min(left, max(left/time.Duration(len(addrs)-i), minShare))
		actx, acancel := context.WithTimeout(ctx, share)
		var c net.Conn
		c, err = dial(actx, "tcp", netip.AddrPortFrom(a, uint16(port)).String())
		acancel()
		if err == nil {
			return c, nil
		}
	}
	if err == nil {
		err = context.DeadlineExceeded
	}
	return nil, fmt.Errorf("cannot connect from the host: %v", err)
}

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

// checkEgressHost reads an OpConnect's host: an IP address, which it
// returns (unmapped) as well, or a DNS name, which it returns in lower case
// without a final dot.
func checkEgressHost(host string) (string, netip.Addr, error) {
	if host == "" || len(host) > agentproto.MaxEgressHost {
		return "", netip.Addr{}, refuse(agentproto.EgressBad, "the host must be a name or an address, of at most %d bytes", agentproto.MaxEgressHost)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return "", netip.Addr{}, refuse(agentproto.EgressBad, "an address with a zone cannot be reached through the host")
		}
		a = a.Unmap()
		return a.String(), a, nil
	}
	name := strings.TrimSuffix(strings.ToLower(host), ".")
	if !validName(name) {
		return "", netip.Addr{}, refuse(agentproto.EgressBad, "%q is not a host name", clip(printableHost(host), 80))
	}
	return name, netip.Addr{}, nil
}

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

// printableHost is host as a log may show it: anything but the bytes of a
// name or an address replaced.
func printableHost(host string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:[]%", r) {
			return r
		}
		return '?'
	}, host)
}

// Allow is egress_allow: names, *.suffix patterns and CIDRs (or single
// addresses) the proxy may reach although they are private.
type Allow struct {
	names    []string
	suffixes []string // ".corp.example", for "*.corp.example"
	nets     []netip.Prefix
}

// ParseAllow reads egress_allow: entries separated by spaces or commas,
// each a host name (git.corp.example), a pattern of every name under one
// (*.corp.example, which does not match corp.example itself), a CIDR
// (10.20.0.0/16) or an address; "" or "none" allows nothing.
func ParseAllow(s string) (Allow, error) {
	var a Allow
	s = strings.TrimSpace(s)
	if s == "none" {
		return a, nil
	}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if strings.Contains(f, "/") {
			p, err := netip.ParsePrefix(f)
			if err != nil || p.Addr().Zone() != "" {
				return Allow{}, fmt.Errorf("egress_allow: %q is not a CIDR", f)
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
				return Allow{}, fmt.Errorf("egress_allow: %q is not a name, *.name, CIDR or address", f)
			}
			a.suffixes = append(a.suffixes, "."+rest)
			continue
		}
		if !validName(name) {
			return Allow{}, fmt.Errorf("egress_allow: %q is not a name, *.name, CIDR or address", f)
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

// halfCloser is c as Splice takes it: a TCP connection already is one;
// another is closed whole when its writing ends.
func halfCloser(c net.Conn) agentproto.HalfCloser {
	if hc, ok := c.(agentproto.HalfCloser); ok {
		return hc
	}
	return wholeCloser{c}
}

type wholeCloser struct{ net.Conn }

func (w wholeCloser) CloseWrite() error { return w.Close() }

// spliceIdle is agentproto.Splice, closing both once nothing has crossed
// either way in idle.
func spliceIdle(a, b agentproto.HalfCloser, idle time.Duration) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTimer(idle)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			since := time.Since(time.Unix(0, last.Load()))
			if since >= idle {
				a.Close()
				b.Close()
				return
			}
			t.Reset(idle - since)
		}
	}()
	agentproto.Splice(&activeConn{a, &last}, &activeConn{b, &last})
}

// activeConn notes when bytes last crossed it.
type activeConn struct {
	agentproto.HalfCloser
	last *atomic.Int64
}

func (c *activeConn) Read(p []byte) (int, error) {
	n, err := c.HalfCloser.Read(p)
	if n > 0 {
		c.last.Store(time.Now().UnixNano())
	}
	return n, err
}

func (c *activeConn) Write(p []byte) (int, error) {
	n, err := c.HalfCloser.Write(p)
	if n > 0 {
		c.last.Store(time.Now().UnixNano())
	}
	return n, err
}
