package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The suite's listener: the one thing an egress check tries to reach. It
// binds only an address the suite chose (127.0.0.1, or the address
// PickLAN chose from this machine's own), on a port the kernel picks,
// never a wildcard; it accepts, reads up to 64 bytes, replies with
// ListenerReply, closes, and counts. It connects to nothing.

const (
	// listenerRead is what it reads of a connection, at most.
	listenerRead = 64
	// listenerConns bounds the connections served at once; one past it is
	// counted and closed unread.
	listenerConns = 16
	// listenerRecords bounds what Received keeps.
	listenerRecords = 256
)

// listenerDeadline bounds a connection's whole life (a variable for the
// tests).
var listenerDeadline = 5 * time.Second

// listenTCP binds; the tests replace it to prove a refused address is
// never bound.
var listenTCP = func(ctx context.Context, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}

// ListenerReply is what the listener answers every connection with.
func ListenerReply(nonce string) string { return "caboose-conformance " + nonce + "\n" }

// TCPListener is a running suite listener.
type TCPListener struct {
	ln    net.Listener
	addr  netip.AddrPort
	reply []byte

	mu       sync.Mutex
	accepted int
	received []string
	closed   bool

	slots chan struct{}
	wg    sync.WaitGroup
	done  chan struct{}
}

// ListenAt starts a listener on addr, which must be 127.0.0.1 or one of
// chosen (the LAN address PickLAN gave); its port is the kernel's. It
// stops when ctx ends or Close is called.
func ListenAt(ctx context.Context, addr netip.Addr, chosen []netip.Addr, nonce string) (*TCPListener, error) {
	ok := addr == netip.MustParseAddr("127.0.0.1")
	for _, c := range chosen {
		if c == addr && listenerAddr(c) {
			ok = true
		}
	}
	if !ok || !listenerAddr(addr) {
		return nil, fmt.Errorf("conformance: %s is not an address the suite chose to listen on", addr)
	}
	if !n8Re.MatchString(firstN(nonce, 8)) {
		return nil, errors.New("conformance: a listener answers with the run's nonce")
	}
	ln, err := listenTCP(ctx, net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		return nil, err
	}
	ap, err := netip.ParseAddrPort(ln.Addr().String())
	if err != nil || ap.Addr().Unmap() != addr || ap.Port() == 0 {
		ln.Close()
		return nil, fmt.Errorf("conformance: the listener is bound to %s, not %s", ln.Addr(), addr)
	}
	l := &TCPListener{ln: ln, addr: netip.AddrPortFrom(addr, ap.Port()), reply: []byte(ListenerReply(nonce)),
		slots: make(chan struct{}, listenerConns), done: make(chan struct{})}
	l.wg.Add(1)
	go l.serve()
	go func() {
		select {
		case <-ctx.Done():
			l.Close()
		case <-l.done:
		}
	}()
	return l, nil
}

func firstN(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func (l *TCPListener) serve() {
	defer l.wg.Done()
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-l.done:
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			c.Close()
			return
		}
		l.accepted++
		l.mu.Unlock()
		select {
		case l.slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			defer func() { <-l.slots }()
			l.handle(c)
		}()
	}
}

func (l *TCPListener) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(listenerDeadline))
	b := readFirstLine(c)
	line, _, _ := strings.Cut(string(b), "\n")
	line = strings.TrimRight(line, "\r")
	l.mu.Lock()
	if len(l.received) < listenerRecords {
		l.received = append(l.received, printableLine(line))
	}
	l.mu.Unlock()
	_, _ = c.Write(l.reply)
}

// readFirstLine reads until a newline, listenerRead bytes, the end of
// the stream or the deadline, whichever comes first.
func readFirstLine(r io.Reader) []byte {
	buf := make([]byte, 0, listenerRead)
	tmp := make([]byte, listenerRead)
	for len(buf) < listenerRead {
		n, err := r.Read(tmp[:listenerRead-len(buf)])
		buf = append(buf, tmp[:n]...)
		if bytes.IndexByte(buf, '\n') >= 0 || err != nil {
			break
		}
	}
	return buf
}

// printableLine keeps a received line's printable ASCII.
func printableLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, s)
}

// Addr is the listener's address and port.
func (l *TCPListener) Addr() string { return l.addr.String() }

// Accepted is how many connections it accepted.
func (l *TCPListener) Accepted() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepted
}

// Received is the first line of each connection served, in order.
func (l *TCPListener) Received() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.received...)
}

// Close stops it and waits for the connections being served; calling it
// again does nothing.
func (l *TCPListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	close(l.done)
	err := l.ln.Close()
	l.wg.Wait()
	return err
}

// Closed reports whether Close has run.
func (l *TCPListener) Closed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// PickLAN is the address a LAN listener binds: the first private IPv4
// address of own (this machine's interface addresses), else the first ULA
// one; never loopback, link-local, a tailnet's (100.64.0.0/10), vmnet's
// subnet (where the guests are) or a public one.
func PickLAN(own []netip.Addr) (netip.Addr, bool) {
	var v6 netip.Addr
	for _, a := range own {
		a = a.Unmap()
		if a.Zone() != "" || !isPrivate(a) || vmnet.Contains(a) {
			continue
		}
		if a.Is4() {
			return a, true
		}
		if !v6.IsValid() {
			v6 = a
		}
	}
	return v6, v6.IsValid()
}

var vmnet = netip.MustParsePrefix("192.168.64.0/24")

// OwnAddrs are this machine's interface addresses, read from its
// interfaces: nothing is sent.
func OwnAddrs() ([]netip.Addr, error) {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range as {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr())
		}
	}
	return out, nil
}

// ListenScope starts the suite's listener at scope: 127.0.0.1, or the
// address PickLAN chooses from own (ErrNoLAN when there is none). It is
// what a real EgressOps.StartListener does.
func ListenScope(ctx context.Context, scope Scope, own []netip.Addr, nonce string) (*TCPListener, error) {
	switch scope {
	case ScopeLoopback:
		return ListenAt(ctx, netip.MustParseAddr("127.0.0.1"), nil, nonce)
	case ScopeLAN:
		a, ok := PickLAN(own)
		if !ok {
			return nil, ErrNoLAN
		}
		return ListenAt(ctx, a, []netip.Addr{a}, nonce)
	}
	return nil, fmt.Errorf("conformance: no listener scope %q", strconv.Quote(string(scope)))
}
