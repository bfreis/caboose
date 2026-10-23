package conformance

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests bind 127.0.0.1 on a port the kernel picks, and connect to
// nothing else.

const testNonce = "0a1b2c3d4e5f60718293a4b5c6d7e8f9"

func startLoopback(t *testing.T) *TCPListener {
	t.Helper()
	l, err := ListenAt(context.Background(), netip.MustParseAddr("127.0.0.1"), nil, testNonce)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func dialSend(t *testing.T, addr, send string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if send != "" {
		if _, err := io.WriteString(c, send); err != nil {
			t.Fatal(err)
		}
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	b, _ := io.ReadAll(io.LimitReader(c, 256))
	return string(b)
}

func TestListenerServes(t *testing.T) {
	l := startLoopback(t)
	ap, err := netip.ParseAddrPort(l.Addr())
	if err != nil || ap.Addr() != netip.MustParseAddr("127.0.0.1") || ap.Port() == 0 {
		t.Fatalf("bound to %s", l.Addr())
	}
	if got := dialSend(t, l.Addr(), "conf-e1-1\n"); got != ListenerReply(testNonce) {
		t.Errorf("reply %q", got)
	}
	// A request line longer than what is read, with no newline in it.
	if got := dialSend(t, l.Addr(), "GET /conf-e1-2 HTTP/1.1"+strings.Repeat("x", 200)); got != ListenerReply(testNonce) {
		t.Errorf("reply %q", got)
	}
	// Nothing sent at all.
	dialSend(t, l.Addr(), "")
	if n := l.Accepted(); n != 3 {
		t.Errorf("accepted %d, want 3", n)
	}
	rec := l.Received()
	if len(rec) != 3 || rec[0] != "conf-e1-1" || !strings.HasPrefix(rec[1], "GET /conf-e1-2 HTTP/1.1") || len(rec[1]) != listenerRead || rec[2] != "" {
		t.Errorf("received %q", rec)
	}
	var _ SuiteListener = l
}

func TestListenerKeepsPrintableASCII(t *testing.T) {
	l := startLoopback(t)
	dialSend(t, l.Addr(), "a\x1b[31mb\xffc\r\n")
	if rec := l.Received(); len(rec) != 1 || rec[0] != "a[31mbc" {
		t.Errorf("received %q", rec)
	}
}

func TestListenerCloses(t *testing.T) {
	l := startLoopback(t)
	addr := l.Addr()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if !l.Closed() {
		t.Error("not closed")
	}
	if err := l.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Error("a closed listener still accepts")
	}
}

func TestListenerClosesWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	l, err := ListenAt(ctx, netip.MustParseAddr("127.0.0.1"), nil, testNonce)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for !l.Closed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !l.Closed() {
		t.Error("the listener outlived its context")
	}
}

// A connection that sends nothing and keeps its end open is closed at the
// deadline; past listenerConns at once, a connection is counted and closed
// unread.
func TestListenerBounds(t *testing.T) {
	old := listenerDeadline
	listenerDeadline = 300 * time.Millisecond
	t.Cleanup(func() { listenerDeadline = old })
	l := startLoopback(t)
	var conns []net.Conn
	t.Cleanup(func() {
		for _, c := range conns {
			c.Close()
		}
	})
	for i := 0; i < listenerConns+1; i++ {
		c, err := net.DialTimeout("tcp", l.Addr(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	deadline := time.Now().Add(2 * time.Second)
	for l.Accepted() < listenerConns+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := l.Accepted(); n != listenerConns+1 {
		t.Fatalf("accepted %d", n)
	}
	// Every one of them is closed by the listener: at its deadline, or at
	// once for the one past the bound.
	for i, c := range conns {
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadAll(c); err != nil {
			t.Errorf("connection %d was not closed by the listener: %v", i, err)
		}
	}
	done := make(chan struct{})
	go func() { l.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
	if n := len(l.Received()); n > listenerConns {
		t.Errorf("%d connections served, at most %d at once", n, listenerConns)
	}
}

// The listener binds 127.0.0.1, or an address the suite chose that is
// private: never a wildcard, a public address, or one it did not choose.
// None of these is bound: each is refused before anything listens.
func TestListenAtRefuses(t *testing.T) {
	old := listenTCP
	listenTCP = func(_ context.Context, addr string) (net.Listener, error) {
		t.Errorf("%s was bound", addr)
		return nil, errors.New("not in a test")
	}
	t.Cleanup(func() { listenTCP = old })
	for _, tc := range []struct {
		addr   string
		chosen []string
	}{
		{"0.0.0.0", nil}, {"::", nil}, {"0.0.0.0", []string{"0.0.0.0"}},
		{"127.0.0.2", nil}, {"::1", nil},
		{"192.168.1.20", nil}, {"192.168.1.20", []string{"192.168.1.21"}},
		{"203.0.113.7", []string{"203.0.113.7"}}, {"8.8.8.8", []string{"8.8.8.8"}},
		{"169.254.1.1", []string{"169.254.1.1"}}, {"fe80::1", []string{"fe80::1"}},
		{"100.64.0.1", []string{"100.64.0.1"}},
	} {
		var chosen []netip.Addr
		for _, c := range tc.chosen {
			chosen = append(chosen, netip.MustParseAddr(c))
		}
		if l, err := ListenAt(context.Background(), netip.MustParseAddr(tc.addr), chosen, testNonce); err == nil {
			l.Close()
			t.Errorf("%s (chosen %v) accepted", tc.addr, tc.chosen)
		}
	}
	if l, err := ListenAt(context.Background(), netip.MustParseAddr("127.0.0.1"), nil, "not-a-nonce"); err == nil {
		l.Close()
		t.Error("a listener without the run's nonce")
	}
}

// A listener the system bound elsewhere than asked is refused and closed.
type elsewhere struct {
	net.Listener
	closed bool
}

func (e *elsewhere) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 40001} }
func (e *elsewhere) Close() error   { e.closed = true; return e.Listener.Close() }

func TestListenAtChecksTheBoundAddress(t *testing.T) {
	old := listenTCP
	var got *elsewhere
	listenTCP = func(ctx context.Context, addr string) (net.Listener, error) {
		ln, err := old(ctx, addr)
		if err != nil {
			return nil, err
		}
		got = &elsewhere{Listener: ln}
		return got, nil
	}
	t.Cleanup(func() { listenTCP = old })
	if l, err := ListenAt(context.Background(), netip.MustParseAddr("127.0.0.1"), nil, testNonce); err == nil {
		l.Close()
		t.Fatal("a listener bound elsewhere accepted")
	}
	if got == nil || !got.closed {
		t.Error("the listener bound elsewhere was not closed")
	}
}

func TestPickLAN(t *testing.T) {
	addrs := func(ss ...string) []netip.Addr {
		var out []netip.Addr
		for _, s := range ss {
			out = append(out, netip.MustParseAddr(s))
		}
		return out
	}
	for _, tc := range []struct {
		own  []netip.Addr
		want string
	}{
		{addrs("127.0.0.1", "::1", "fe80::1%en0"), ""},
		{addrs("127.0.0.1", "192.168.64.1", "100.100.1.1", "203.0.113.5"), ""},
		{addrs("127.0.0.1", "192.168.64.1", "192.168.1.20", "10.0.0.5"), "192.168.1.20"},
		{addrs("fd12::5", "10.1.2.3"), "10.1.2.3"},
		{addrs("fd12::5", "fe80::1", "2001:db8::1"), "fd12::5"},
		{addrs("::ffff:172.16.0.9"), "172.16.0.9"},
	} {
		got, ok := PickLAN(tc.own)
		if tc.want == "" {
			if ok {
				t.Errorf("%v: picked %s", tc.own, got)
			}
			continue
		}
		if !ok || got.String() != tc.want {
			t.Errorf("%v: picked %s %v, want %s", tc.own, got, ok, tc.want)
		}
	}
}

func TestListenScope(t *testing.T) {
	ctx := context.Background()
	for _, own := range [][]netip.Addr{nil, {netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("8.8.8.8")}} {
		if _, err := ListenScope(ctx, ScopeLAN, own, testNonce); !errors.Is(err, ErrNoLAN) {
			t.Errorf("LAN with %v: %v, want ErrNoLAN", own, err)
		}
	}
	if _, err := ListenScope(ctx, "public", nil, testNonce); err == nil {
		t.Error("a scope the suite has not accepted")
	}
	l, err := ListenScope(ctx, ScopeLoopback, nil, testNonce)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !strings.HasPrefix(l.Addr(), "127.0.0.1:") {
		t.Errorf("loopback at %s", l.Addr())
	}
}

// OwnAddrs reads the interfaces and sends nothing.
func TestOwnAddrs(t *testing.T) {
	as, err := OwnAddrs()
	if err != nil {
		t.Skip(err)
	}
	for _, a := range as {
		if !a.IsValid() {
			t.Errorf("an invalid address: %v", a)
		}
	}
}

type addrListener struct{ addr string }

func (a addrListener) Addr() string     { return a.addr }
func (addrListener) Accepted() int      { return 0 }
func (addrListener) Close() error       { return nil }
func (addrListener) Received() []string { return nil }

func TestTargetOf(t *testing.T) {
	for _, bad := range []string{"203.0.113.7:443", "8.8.8.8:53", "0.0.0.0:80", "127.0.0.2:80", "[::1]:80",
		"192.168.1.20:0", "192.168.1.20", "localhost:80", "[fe80::1%en0]:80", "[::ffff:10.0.0.1]:80", "169.254.169.254:80"} {
		if _, err := TargetOf(addrListener{bad}); err == nil {
			t.Errorf("%s is a target", bad)
		}
	}
	if _, err := TargetOf(nil); err == nil {
		t.Error("no listener is a target")
	}
	lo, err := TargetOf(addrListener{"127.0.0.1:40001"})
	if err != nil || lo.Host() != "127.0.0.1" || lo.Port() != 40001 || lo.String() != "127.0.0.1:40001" {
		t.Fatalf("loopback: %v %v", lo, err)
	}
	n, err := lo.ByName()
	if err != nil || n.Host() != "localhost" || n.hostPort() != "localhost:40001" {
		t.Errorf("by name: %v %v", n, err)
	}
	if _, err := lo.CIDR(); err == nil {
		t.Error("loopback has a network")
	}
	lan, err := TargetOf(addrListener{"192.168.1.20:40002"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lan.ByName(); err == nil {
		t.Error("the LAN listener has a name")
	}
	if cidr, err := lan.CIDR(); err != nil || cidr != "192.168.1.0/24" {
		t.Errorf("cidr %s %v", cidr, err)
	}
	v6, err := TargetOf(addrListener{"[fd12::5]:40003"})
	if err != nil || v6.hostPort() != "[fd12::5]:40003" {
		t.Fatalf("v6: %v %v", v6, err)
	}
	if cidr, _ := v6.CIDR(); cidr != "fd12::/64" {
		t.Errorf("v6 cidr %s", cidr)
	}
	doc, err := DocTarget(lan)
	if err != nil || doc.String() != "192.0.2.1:40002" {
		t.Errorf("doc %v %v", doc, err)
	}
	if _, err := DocTarget(Target{}); err == nil {
		t.Error("a doc target beside nothing")
	}
}

func TestEgressBuilders(t *testing.T) {
	lo, _ := TargetOf(addrListener{"127.0.0.1:40001"})
	lan, _ := TargetOf(addrListener{"192.168.1.20:40002"})
	v6, _ := TargetOf(addrListener{"[fd12::5]:40003"})
	byName, _ := lo.ByName()
	tok, err := Token("E1", 3)
	if err != nil || tok != "conf-e1-3" {
		t.Fatalf("token %q %v", tok, err)
	}
	mk := func(c Cmd, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if c.Destructive() {
			t.Errorf("%q is destructive", c.Body())
		}
		return c.Body()
	}
	want := map[string]string{
		mk(AgentConnect(byName, tok)): "echo 'conf-e1-3' | caboose-agent connect 'localhost' '40001' 1>&2",
		mk(AgentConnect(v6, tok)):     "echo 'conf-e1-3' | caboose-agent connect 'fd12::5' '40003' 1>&2",
		mk(CurlProxy(lan, tok)):       "curl -g -sS -f --max-time 20 --noproxy '' --proxy 'http://127.0.0.1:9128' 'http://192.168.1.20:40002/conf-e1-3' 1>&2",
		mk(CurlProxy(v6, tok)):        "curl -g -sS -f --max-time 20 --noproxy '' --proxy 'http://127.0.0.1:9128' 'http://[fd12::5]:40003/conf-e1-3' 1>&2",
		mk(CurlDirect(lan, tok)):      "curl -g -sS -f --max-time 10 --noproxy '192.168.1.20' 'http://192.168.1.20:40002/conf-e1-3' 1>&2",
		mk(Ping(lan)):                 "ping -c 1 -W 2 '192.168.1.20' 1>&2",
		Routes().Body():               "{ ip route show table all 2>/dev/null || ip route show; } 1>&2 && if [ -e /proc/net/if_inet6 ]; then { ip -6 route show table all 2>/dev/null || ip -6 route show; } 1>&2; else echo 'conformance: no ipv6' 1>&2; fi",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("got  %q\nwant %q", got, w)
		}
		standIn := strings.ReplaceAll(got, "caboose-agent", "conf-agent-stand-in")
		if out, err := exec.Command("sh", "-n", "-c", wrapPrefix+standIn+wrapSuffix).CombinedOutput(); err != nil {
			t.Errorf("sh -n rejected %q: %v %s", standIn, err, out)
		}
	}
	for _, bad := range []func() error{
		func() error { _, err := AgentConnect(Target{}, tok); return err },
		func() error { _, err := AgentConnect(lan, "x'; id"); return err },
		func() error { _, err := CurlProxy(lan, "conf-e1-"); return err },
		func() error {
			_, err := CurlDirect(Target{addr: netip.MustParseAddr("8.8.8.8"), port: 80}, tok)
			return err
		},
		func() error { _, err := Ping(byName); return err },
		func() error { _, err := Ping(Target{}); return err },
		func() error { _, err := Token("E1 x", 1); return err },
	} {
		if bad() == nil {
			t.Error("a builder accepted what it must refuse")
		}
	}
}

func TestEgressPolicyEqual(t *testing.T) {
	a := EgressPolicy{Allow: []string{"x"}}
	if !a.Equal(EgressPolicy{Allow: []string{"x"}}) {
		t.Error("equal policies differ")
	}
	if a.Equal(EgressPolicy{Allow: []string{"x"}, Ports: []int{}}) {
		t.Error("unset ports equal empty ones")
	}
	if a.Equal(EgressPolicy{Allow: []string{"x"}, Deny: []string{"x"}}) {
		t.Error("deny ignored")
	}
}

func TestEgressOpsOutsideTheEnvelope(t *testing.T) {
	var env Env
	if _, err := env.EgressOps(); !errors.Is(err, ErrNoEnvelope) {
		t.Errorf("EgressOps outside the envelope: %v", err)
	}
}
