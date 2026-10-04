package hostlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
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

func TestParseEgressPorts(t *testing.T) {
	set, err := ParsePortsOf("egress_ports", "22 80 443")
	if err != nil || !set.Has(22) || !set.Has(443) || set.Has(25) {
		t.Fatalf("%v %v", set, err)
	}
	if _, err := ParsePortsOf("egress_ports", "443 0"); err == nil || !strings.Contains(err.Error(), "egress_ports") {
		t.Errorf("a bad port: %v", err)
	}
}

func TestCheckEgressHost(t *testing.T) {
	for in, want := range map[string]string{
		"GitHub.com.": "github.com", "a_b-c.example": "a_b-c.example",
		"::ffff:1.2.3.4": "1.2.3.4", "2606:4700::1": "2606:4700::1", "1.2.3.4": "1.2.3.4",
	} {
		if got, _, err := checkEgressHost(in); err != nil || got != want {
			t.Errorf("checkEgressHost(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "a b", "[::1]", "fe80::1%en0", "x\x1b[31m", "a..b", ".", "host:443", strings.Repeat("a.", 127) + "aa", "é.example"} {
		if _, _, err := checkEgressHost(bad); egressReason(err) != agentproto.EgressBad {
			t.Errorf("checkEgressHost(%q): %v", bad, err)
		}
	}
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
		{"*.example", []netip.Addr{priv}, false},
		{"10.0.0.0/24", []netip.Addr{pub, priv}, false},
		{"10.0.1.0/24", []netip.Addr{pub, priv}, true},
		{"other.example", []netip.Addr{priv}, true},
	}
	for _, c := range cases {
		allow, err := ParseAllow(c.allow)
		if err != nil {
			t.Fatal(err)
		}
		e := &Egress{Allow: allow}
		if _, denied := e.denied("rebind.example", c.addrs, nil); denied != c.denied {
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
		allow, err := ParseAllow(c.allow)
		if err != nil {
			t.Fatal(err)
		}
		e := &Egress{Allow: allow}
		if _, denied := e.denied("x.nip.io", []netip.Addr{netip.MustParseAddr(c.addr)}, own); denied != c.denied {
			t.Errorf("allow %q, %s: denied %v", c.allow, c.addr, denied)
		}
	}
	// A literal is never allowed by a pattern, whatever its digits.
	allow, _ := ParseAllow("*.0.7")
	e := &Egress{Allow: allow}
	if _, denied := e.denied("", []netip.Addr{netip.MustParseAddr("10.0.0.7")}, nil); !denied {
		t.Error("an address literal allowed by a pattern")
	}
}

// This machine's addresses are read at most every egressOwnEvery, and a
// name resolving to one is refused.
func TestEgressOwnAddrs(t *testing.T) {
	var reads int
	ownAddr := netip.MustParseAddr("203.0.113.9")
	fn := &fakeNet{echo: echoServer(t), names: map[string][]netip.Addr{"me.example": {ownAddr}}}
	ports, _ := ParsePortsOf("egress_ports", "443")
	e := &Egress{Ports: ports, Resolver: fn, Dial: fn.Dial, Own: func() ([]netip.Addr, error) {
		reads++
		return []netip.Addr{netip.MustParseAddr("::ffff:203.0.113.9")}, nil
	}}
	h := &Host{cfg: Config{Log: log.New(io.Discard, "", 0), Egress: e}}
	for i := 0; i < 3; i++ {
		if err := h.connect(1, "me.example", 443); egressReason(err) != agentproto.EgressDenied {
			t.Fatalf("this machine's own address: %v", err)
		}
	}
	if reads != 1 {
		t.Errorf("read %d times", reads)
	}
	h.egress.ownAt = time.Now().Add(-egressOwnEvery)
	_ = h.connect(1, "me.example", 443)
	if reads != 2 {
		t.Errorf("not read again once stale: %d", reads)
	}
	if len(fn.dialled) != 0 {
		t.Errorf("dialled %v", fn.dialled)
	}
	// The interfaces' own, as the launcher reads them, hold loopback.
	if own, err := interfaceAddrs(); err != nil || !slices.ContainsFunc(own, netip.Addr.IsLoopback) {
		t.Errorf("interfaceAddrs: %v %v", own, err)
	}
}

// An address that never answers leaves time for the next.
func TestEgressDialShares(t *testing.T) {
	echo := echoServer(t)
	var mu sync.Mutex
	var tried []string
	e := &Egress{dialTimeout: 900 * time.Millisecond, minShare: 100 * time.Millisecond,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			tried = append(tried, addr)
			mu.Unlock()
			if strings.HasPrefix(addr, "192.0.2.") { // blackholed
				<-ctx.Done()
				return nil, ctx.Err()
			}
			var d net.Dialer
			return d.DialContext(ctx, network, echo)
		}}
	addrs := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("198.51.100.1")}
	start := time.Now()
	c, err := e.dial(addrs, 443)
	if err != nil {
		t.Fatalf("the third address never got its turn: %v (tried %v)", err, tried)
	}
	c.Close()
	if d := time.Since(start); d > 900*time.Millisecond {
		t.Errorf("took %v", d)
	}
	if len(tried) != 3 {
		t.Errorf("tried %v", tried)
	}
	// None answers: the whole budget, and no more.
	start = time.Now()
	if _, err := e.dial(addrs[:2], 443); err == nil {
		t.Fatal("a blackhole answered")
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("took %v", d)
	}
}

// The guest decides how often a refusal happens: link.log gets at most
// egressLogMax lines a period, then one line with the count of the rest.
func TestEgressLogLimit(t *testing.T) {
	var logged syncBuffer
	ports, _ := ParsePortsOf("egress_ports", "443")
	h := &Host{cfg: Config{Log: log.New(&logged, "", 0), Egress: &Egress{Ports: ports}}}
	for i := 0; i < 50; i++ {
		_ = h.connect(1, "example.com", 25)
	}
	if n := strings.Count(logged.String(), "\n"); n != egressLogMax {
		t.Fatalf("%d lines:\n%s", n, logged.String())
	}
	h.mu.Lock()
	h.egress.logFrom = time.Now().Add(-egressLogEvery)
	h.mu.Unlock()
	_ = h.connect(1, "example.com", 25)
	got := logged.String()
	if !strings.Contains(got, "and 40 more refused") || strings.Count(got, "\n") != egressLogMax+2 {
		t.Errorf("after the period:\n%s", got)
	}
}

func TestEgressLimits(t *testing.T) {
	h := &Host{cfg: Config{Log: log.New(io.Discard, "", 0)}}
	for i := 0; i < egressBurst; i++ {
		if err := h.takeEgress(); err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
	}
	if err := h.takeEgress(); egressReason(err) != agentproto.EgressLimit {
		t.Fatalf("past the burst: %v", err)
	}
	// Time refills the bucket, but no more than maxEgress are open.
	for h.egress.open < maxEgress {
		h.egress.last = time.Now().Add(-time.Hour)
		if err := h.takeEgress(); err != nil {
			t.Fatalf("connection %d: %v", h.egress.open, err)
		}
	}
	h.egress.last = time.Now().Add(-time.Hour)
	if err := h.takeEgress(); err == nil || !strings.Contains(err.Error(), "already 128") {
		t.Fatalf("past maxEgress: %v", err)
	}
	h.releaseEgress()
	if err := h.takeEgress(); err != nil {
		t.Fatalf("after one closed: %v", err)
	}
}

// A connection nothing crosses for idle is closed; one in use is not.
func TestSpliceIdle(t *testing.T) {
	a, aPeer := net.Pipe()
	b, bPeer := net.Pipe()
	done := make(chan struct{})
	go func() {
		spliceIdle(wholeCloser{a}, wholeCloser{b}, 100*time.Millisecond)
		close(done)
	}()
	go io.Copy(io.Discard, bPeer)
	for i := 0; i < 10; i++ {
		if _, err := aPeer.Write([]byte("x")); err != nil {
			t.Fatalf("closed while in use, after %d writes: %v", i, err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("closed while in use")
	default:
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an idle connection stays open")
	}
}

// fakeNet resolves from a table and dials an echo server, whatever the
// address, noting each.
type fakeNet struct {
	mu       sync.Mutex
	names    map[string][]netip.Addr
	lookups  []string
	dialled  []string
	failDial bool
	echo     string
}

func (f *fakeNet) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups = append(f.lookups, host)
	addrs, ok := f.names[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return addrs, nil
}

func (f *fakeNet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	f.mu.Lock()
	f.dialled = append(f.dialled, addr)
	fail := f.failDial
	f.mu.Unlock()
	if fail {
		return nil, errors.New("connection refused")
	}
	var d net.Dialer
	return d.DialContext(ctx, network, f.echo)
}

func echoServer(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// fakeAgent is the agent's end of a link, driven by the test.
type fakeAgent struct {
	t     *testing.T
	sess  *agentproto.Session
	hello agentproto.Message
	next  uint64
}

func egressLinked(t *testing.T, cfg Config) *fakeAgent {
	t.Helper()
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	host := agentproto.NewSession(hr, hw, true)
	sess := agentproto.NewSession(ar, aw, false)
	t.Cleanup(func() { host.Close(); sess.Close() })
	var logged bytes.Buffer
	if cfg.Log == nil {
		cfg.Log = log.New(&logged, "", 0)
	}
	go Run(host, cfg)
	a := &fakeAgent{t: t, sess: sess}
	a.hello = a.control()
	if err := sess.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version}); err != nil {
		t.Fatal(err)
	}
	return a
}

func (a *fakeAgent) control() agentproto.Message {
	a.t.Helper()
	select {
	case b := <-a.sess.Control():
		m, err := agentproto.Decode(b)
		if err != nil {
			a.t.Fatal(err)
		}
		return m
	case <-time.After(5 * time.Second):
		a.t.Fatal("no message from the host")
	}
	return agentproto.Message{}
}

// connect asks for host:port and returns the host's response.
func (a *fakeAgent) connect(host string, port int) agentproto.Message {
	a.t.Helper()
	a.next++
	if err := a.sess.Send(agentproto.Message{Type: agentproto.TypeRequest, ID: a.next, Op: agentproto.OpConnect, Host: host, Port: port}); err != nil {
		a.t.Fatal(err)
	}
	for {
		m := a.control()
		if m.Type == agentproto.TypeResponse && m.ID == a.next {
			return m
		}
	}
}

func TestEgressRoundTrip(t *testing.T) {
	fn := &fakeNet{echo: echoServer(t), names: map[string][]netip.Addr{
		"github.com":     {netip.MustParseAddr("140.82.112.3")},
		"localhost":      {netip.MustParseAddr("127.0.0.1")},
		"rebind.example": {netip.MustParseAddr("140.82.112.4"), netip.MustParseAddr("192.168.64.1")},
		"git.corp":       {netip.MustParseAddr("10.1.2.3")},
		"down.example":   {netip.MustParseAddr("140.82.112.5")},
	}}
	ports, _ := ParsePortsOf("egress_ports", "22 80 443")
	allow, _ := ParseAllow("git.corp")
	a := egressLinked(t, Config{Egress: &Egress{Ports: ports, Allow: allow, Resolver: fn, Dial: fn.Dial}})
	if a.hello.Egress != agentproto.EgressListen {
		t.Fatalf("the hello offers %q", a.hello.Egress)
	}

	// Allowed: a stream answers the request, and carries bytes both ways.
	r := a.connect("GitHub.com", 443)
	if !r.OK {
		t.Fatalf("github.com:443 refused: %+v", r)
	}
	st, err := a.sess.Accept()
	if err != nil {
		t.Fatal(err)
	}
	var hdr agentproto.StreamHeader
	if err := json.Unmarshal(st.Header(), &hdr); err != nil || hdr.Request != r.ID {
		t.Fatalf("stream header %s for request %d", st.Header(), r.ID)
	}
	if _, err := st.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
	st.Close()

	// An allowed private name, by egress_allow.
	if r := a.connect("git.corp", 22); !r.OK {
		t.Errorf("git.corp:22 refused: %+v", r)
	} else if st, err := a.sess.Accept(); err == nil {
		st.Close()
	}

	for _, c := range []struct {
		host   string
		port   int
		reason string
	}{
		{"github.com", 25, agentproto.EgressPort},
		{"localhost", 443, agentproto.EgressDenied},
		{"rebind.example", 443, agentproto.EgressDenied},
		{"::ffff:127.0.0.1", 443, agentproto.EgressDenied},
		{"10.0.0.1", 80, agentproto.EgressDenied},
		{"a b", 443, agentproto.EgressBad},
		{"github.com", 70000, agentproto.EgressBad},
		{"nowhere.example", 443, agentproto.EgressResolve},
	} {
		r := a.connect(c.host, c.port)
		if r.OK || r.Reason != c.reason || r.Error == "" {
			t.Errorf("%s:%d: %+v, want reason %q", c.host, c.port, r, c.reason)
		}
	}
	if r := a.connect("github.com", 25); !strings.Contains(r.Error, "egress_ports") {
		t.Errorf("the port's refusal says %q", r.Error)
	}

	fn.mu.Lock()
	fn.failDial = true
	fn.mu.Unlock()
	if r := a.connect("down.example", 443); r.OK || r.Reason != agentproto.EgressDial || !strings.Contains(r.Error, "connection refused") {
		t.Errorf("a failed dial: %+v", r)
	}

	fn.mu.Lock()
	defer fn.mu.Unlock()
	// Only checked addresses were dialled, as addresses; a literal was
	// never looked up.
	want := []string{"140.82.112.3:443", "10.1.2.3:22", "140.82.112.5:443"}
	if strings.Join(fn.dialled, " ") != strings.Join(want, " ") {
		t.Errorf("dialled %v, want %v", fn.dialled, want)
	}
	for _, l := range fn.lookups {
		if l == "::ffff:127.0.0.1" || l == "10.0.0.1" || l == "a b" {
			t.Errorf("looked up %q", l)
		}
	}
}

// With egress_proxy off, the hello offers no proxy and a request is
// refused as off.
func TestEgressOff(t *testing.T) {
	a := egressLinked(t, Config{})
	if a.hello.Egress != "" {
		t.Fatalf("the hello offers %q", a.hello.Egress)
	}
	if r := a.connect("github.com", 443); r.OK || r.Reason != agentproto.EgressOff {
		t.Errorf("%+v", r)
	}
}

// link.log gets the target and why, never contents, and the hourly count.
func TestEgressLog(t *testing.T) {
	var logged syncBuffer
	fn := &fakeNet{echo: echoServer(t), names: map[string][]netip.Addr{"localhost": {netip.MustParseAddr("127.0.0.1")}}}
	ports, _ := ParsePortsOf("egress_ports", "443")
	h := &Host{cfg: Config{Log: log.New(&logged, "", 0), Egress: &Egress{Ports: ports, Resolver: fn, Dial: fn.Dial}}}
	for _, c := range []struct {
		host string
		port int
	}{{"localhost", 443}, {"example.com", 25}, {"x\x1b[2J", 443}} {
		_ = h.connect(1, c.host, c.port)
	}
	got := logged.String()
	for _, want := range []string{"egress localhost:443: refused: resolves to 127.0.0.1", "egress example.com:25: refused: not in egress_ports", "egress x?[2J: refused"} {
		if !strings.Contains(got, want) {
			t.Errorf("link.log lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("link.log holds an escape:\n%q", got)
	}
	h.countEgress("github.com:443")
	h.countEgress("github.com:443")
	h.countEgress("example.com:80")
	h.logEgressCounts()
	if !strings.Contains(logged.String(), "example.com:80 1, github.com:443 2") {
		t.Errorf("counts:\n%s", logged.String())
	}
	if h.egress.open != 0 {
		t.Errorf("%d slots held after refusals", h.egress.open)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
