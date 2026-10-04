package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	testHW   = net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}
	testAddr = net.IPv4(192, 168, 64, 7).To4()
)

func TestDHCPRenewal(t *testing.T) {
	p, err := dhcpRenewal(0xdeadbeef, testAddr, testHW)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 300 || p[0] != 1 || p[1] != 1 || p[2] != 6 {
		t.Fatalf("header: len %d, % x", len(p), p[:4])
	}
	if binary.BigEndian.Uint32(p[4:]) != 0xdeadbeef {
		t.Error("xid")
	}
	if !net.IP(p[12:16]).Equal(testAddr) {
		t.Errorf("ciaddr %v", net.IP(p[12:16]))
	}
	for _, f := range [][]byte{p[16:20], p[20:24], p[24:28]} {
		if !net.IP(f).Equal(net.IPv4zero) {
			t.Errorf("yiaddr, siaddr and giaddr are 0 in a request: %v", net.IP(f))
		}
	}
	if net.HardwareAddr(p[28:34]).String() != testHW.String() {
		t.Errorf("chaddr %v", net.HardwareAddr(p[28:34]))
	}
	if binary.BigEndian.Uint16(p[10:]) != 0 {
		t.Error("the broadcast flag is set: a renewing client takes a unicast reply")
	}
	opts := map[byte][]byte{}
	for o := p[240:]; len(o) > 0 && o[0] != optEnd; o = o[2+int(o[1]):] {
		opts[o[0]] = o[2 : 2+int(o[1])]
	}
	if string(opts[optMsgType]) != string([]byte{dhcpRequest}) {
		t.Errorf("message type %v", opts[optMsgType])
	}
	for _, o := range []byte{50, optServerID, 61} {
		if _, ok := opts[o]; ok {
			t.Errorf("option %d in a renewal", o)
		}
	}
	if _, err := dhcpRenewal(1, net.ParseIP("fe80::1"), testHW); err == nil {
		t.Error("an IPv6 address accepted")
	}
}

// reply is a server's reply to xid: a message type, and a lease in
// seconds unless it is negative.
func reply(xid uint32, yiaddr net.IP, msg byte, lease int64) []byte {
	p := make([]byte, 240)
	p[0], p[1], p[2] = 2, 1, 6
	binary.BigEndian.PutUint32(p[4:], xid)
	copy(p[16:20], yiaddr.To4())
	copy(p[236:], dhcpMagic)
	p = append(p, 0, optMsgType, 1, msg, optServerID, 4, 192, 168, 64, 1)
	if lease >= 0 {
		p = append(p, optLeaseTime, 4)
		p = binary.BigEndian.AppendUint32(p, uint32(lease))
	}
	return append(p, optEnd)
}

func TestParseDHCPReply(t *testing.T) {
	l, err := parseDHCPReply(reply(7, testAddr, dhcpACK, 86400), 7)
	if err != nil || l.Lease != 24*time.Hour || !l.Addr.Equal(testAddr) {
		t.Errorf("ACK: %+v, %v", l, err)
	}
	if l, err := parseDHCPReply(reply(7, testAddr, dhcpACK, 0xffffffff), 7); err != nil || l.Lease != 0 {
		t.Errorf("infinite: %+v, %v", l, err)
	}
	if _, err := parseDHCPReply(reply(7, testAddr, dhcpNAK, -1), 7); !errors.Is(err, errNAK) {
		t.Errorf("NAK: %v", err)
	}
	if _, err := parseDHCPReply(reply(8, testAddr, dhcpACK, 60), 7); err == nil {
		t.Error("another xid's reply taken")
	}
	if _, err := parseDHCPReply(reply(7, testAddr, 2, 60), 7); err == nil {
		t.Error("an OFFER taken for an ACK")
	}
	bad := reply(7, testAddr, dhcpACK, 60)
	if _, err := parseDHCPReply(bad[:len(bad)-4], 7); err == nil {
		t.Error("a truncated option accepted")
	}
	if _, err := parseDHCPReply(bad[:200], 7); err == nil {
		t.Error("a short packet accepted")
	}
}

func TestPNPServer(t *testing.T) {
	pnp := "#PROTO: DHCP\ndomain local\nnameserver 192.168.64.1\nbootserver 192.168.64.1\n"
	if s := pnpServer(pnp); !s.Equal(net.IPv4(192, 168, 64, 1)) {
		t.Errorf("server %v", s)
	}
	if s := pnpServer("#PROTO: DHCP\nbootserver 0.0.0.0\n"); s != nil {
		t.Errorf("0.0.0.0 taken for a server: %v", s)
	}
}

// fakeDHCP answers renewals on a UDP port: each request is handed to
// answer, whose packets go back in order (nil: none). It records the
// requests.
type fakeDHCP struct {
	conn net.PacketConn
	mu   sync.Mutex
	reqs [][]byte
}

func newFakeDHCP(t *testing.T, answer func(n int, xid uint32) [][]byte) *fakeDHCP {
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	f := &fakeDHCP{conn: c}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.reqs = append(f.reqs, append([]byte(nil), buf[:n]...))
			k := len(f.reqs)
			f.mu.Unlock()
			for _, p := range answer(k, binary.BigEndian.Uint32(buf[4:])) {
				c.WriteTo(p, from)
			}
		}
	}()
	return f
}

func (f *fakeDHCP) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func testClient(t *testing.T, f *fakeDHCP) *dhcpClient {
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &dhcpClient{conn: c, server: f.conn.LocalAddr(), addr: testAddr, hw: testHW, wait: 200 * time.Millisecond, tries: 3}
}

// A stray reply (another xid) is skipped, and the ACK after it taken.
func TestRenewACK(t *testing.T) {
	f := newFakeDHCP(t, func(_ int, xid uint32) [][]byte {
		return [][]byte{reply(xid+1, testAddr, dhcpACK, 5), []byte("junk"), reply(xid, testAddr, dhcpACK, 600)}
	})
	l, err := testClient(t, f).renew()
	if err != nil || l.Lease != 10*time.Minute {
		t.Fatalf("%+v, %v", l, err)
	}
	if f.requests() != 1 {
		t.Errorf("%d requests for one ACK", f.requests())
	}
}

// Lost requests are sent again, with a new xid, up to tries.
func TestRenewRetries(t *testing.T) {
	f := newFakeDHCP(t, func(n int, xid uint32) [][]byte {
		if n < 3 {
			return nil
		}
		return [][]byte{reply(xid, testAddr, dhcpACK, 600)}
	})
	if _, err := testClient(t, f).renew(); err != nil {
		t.Fatal(err)
	}
	var xids []uint32
	f.mu.Lock()
	for _, r := range f.reqs {
		xids = append(xids, binary.BigEndian.Uint32(r[4:]))
	}
	f.mu.Unlock()
	if len(xids) != 3 || xids[0] == xids[1] || xids[1] == xids[2] {
		t.Errorf("xids %v", xids)
	}

	silent := newFakeDHCP(t, func(int, uint32) [][]byte { return nil })
	if _, err := testClient(t, silent).renew(); err == nil || !strings.Contains(err.Error(), "no reply") {
		t.Errorf("no server: %v", err)
	}
	if silent.requests() != 3 {
		t.Errorf("%d requests, not 3", silent.requests())
	}
}

func TestRenewNAK(t *testing.T) {
	f := newFakeDHCP(t, func(_ int, xid uint32) [][]byte { return [][]byte{reply(xid, testAddr, dhcpNAK, -1)} })
	if _, err := testClient(t, f).renew(); !errors.Is(err, errNAK) {
		t.Errorf("%v", err)
	}
	if f.requests() != 1 {
		t.Errorf("a NAK retried: %d requests", f.requests())
	}
}

// logs collects keepLease's lines.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) wait(t *testing.T, n int) []string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		l.mu.Lock()
		got := append([]string(nil), l.lines...)
		l.mu.Unlock()
		if len(got) >= n {
			return got
		}
	}
	t.Fatalf("fewer than %d lines", n)
	return nil
}

// keepLease renews at once, says the lease, then waits (at least
// dhcpMinWait) until done.
func TestKeepLease(t *testing.T) {
	f := newFakeDHCP(t, func(_ int, xid uint32) [][]byte { return [][]byte{reply(xid, testAddr, dhcpACK, 86400)} })
	var l logs
	done := make(chan struct{})
	ended := make(chan struct{})
	go func() { keepLease(testClient(t, f), l.logf, done, 10*time.Millisecond); close(ended) }()
	got := l.wait(t, 1)
	if !strings.Contains(got[0], "for 24h0m0s, MAC 02:11:22:33:44:55; renewing every 12h0m0s") {
		t.Errorf("%q", got[0])
	}
	time.Sleep(100 * time.Millisecond)
	close(done)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("keepLease did not end with done")
	}
	if f.requests() != 1 {
		t.Errorf("%d requests within a lease's first half", f.requests())
	}
}

func TestKeepLeaseFailure(t *testing.T) {
	f := newFakeDHCP(t, func(int, uint32) [][]byte { return nil })
	var l logs
	done := make(chan struct{})
	defer close(done)
	c := testClient(t, f)
	c.wait = 50 * time.Millisecond
	go keepLease(c, l.logf, done, 10*time.Millisecond)
	got := l.wait(t, 1)
	if !strings.Contains(got[0], "renewing 192.168.64.7 failed") || !strings.Contains(got[0], "never learned") {
		t.Errorf("%q", got[0])
	}
}

func TestKeepLeaseInfinite(t *testing.T) {
	f := newFakeDHCP(t, func(_ int, xid uint32) [][]byte { return [][]byte{reply(xid, testAddr, dhcpACK, 0xffffffff)} })
	var l logs
	ended := make(chan struct{})
	go func() { keepLease(testClient(t, f), l.logf, make(chan struct{}), 10*time.Millisecond); close(ended) }()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("keepLease went on after an infinite lease")
	}
	if got := l.wait(t, 1); !strings.Contains(got[0], "infinite") {
		t.Errorf("%q", got[0])
	}
}
