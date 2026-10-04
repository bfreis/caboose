package agent

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// The guest's DHCP lease, kept. A vm guest takes its address from the
// kernel's own client (ip=dhcp), which asks once at boot and never
// renews. On a Mac, VZNATNetworkDeviceAttachment's DHCP is the system's
// bootpd (InternetSharing's), which leases for 86400 s by default, 600 s
// on a Mac set up as Tart's FAQ suggests (DHCPLeaseTimeSecs), and hands
// an expired lease to a new client once its pool of about 253 addresses
// is used up. A NIC with a random MAC is a new client at every boot,
// taking an address of its own for a whole lease (caboose's VMs now have
// a MAC of their own, vm.Dir.MAC, but other tools' VMs on vmnet may not).
// A sandbox that runs for days, on a Mac that boots VMs often, would then
// find its address given to another VM, and lose its network with
// nothing on the console to say why. So the agent renews it, as a client in the
// RENEWING state does (RFC 2131 4.3.2): a DHCPREQUEST unicast to the
// server the kernel took its answer from, for the address it has, once at
// start, to learn the lease the kernel does not keep, then at half of
// each lease. It never changes the address: a NAK, or an ACK for another
// one, is said on the console, and the renewals go on. Nothing waits for
// it, and it ends with Shutdown.

// dhcpLease is what a server's DHCPACK said.
type dhcpLease struct {
	Addr  net.IP        // yiaddr
	Lease time.Duration // option 51; 0 when the lease is infinite or absent
}

// errNAK is a server's DHCPNAK: it will not renew the address.
var errNAK = errors.New("the DHCP server refused the address (DHCPNAK)")

const (
	dhcpRequest = 3
	dhcpACK     = 5
	dhcpNAK     = 6

	optLeaseTime = 51
	optMsgType   = 53
	optServerID  = 54
	optParams    = 55
	optEnd       = 255
)

var dhcpMagic = []byte{99, 130, 83, 99}

// dhcpRenewal is a RENEWING client's DHCPREQUEST for addr, from the NIC
// whose MAC is hw, with transaction id xid. It carries neither a requested
// address nor a server id (RFC 2131 4.3.2: ciaddr says which address),
// nor a client id, which the kernel's client sends none of either: the
// server finds the lease by hw.
func dhcpRenewal(xid uint32, addr net.IP, hw net.HardwareAddr) ([]byte, error) {
	ip := addr.To4()
	if ip == nil || len(hw) != 6 {
		return nil, fmt.Errorf("a renewal needs an IPv4 address and an Ethernet MAC (have %v, %v)", addr, hw)
	}
	p := make([]byte, 240, 300)
	p[0] = 1 // BOOTREQUEST
	p[1] = 1 // Ethernet
	p[2] = 6
	binary.BigEndian.PutUint32(p[4:], xid)
	copy(p[12:16], ip) // ciaddr
	copy(p[28:34], hw) // chaddr
	copy(p[236:240], dhcpMagic)
	p = append(p, optMsgType, 1, dhcpRequest)
	p = append(p, optParams, 4, 1, 3, 6, optLeaseTime)
	p = append(p, optEnd)
	for len(p) < 300 { // the BOOTP minimum some servers still want
		p = append(p, 0)
	}
	return p, nil
}

// parseDHCPReply is the lease in a server's reply to xid, errNAK for a
// NAK, or an error for anything else (a reply to another request among
// them, which the caller skips).
func parseDHCPReply(p []byte, xid uint32) (dhcpLease, error) {
	var l dhcpLease
	if len(p) < 240 || p[0] != 2 || string(p[236:240]) != string(dhcpMagic) {
		return l, errors.New("not a DHCP reply")
	}
	if binary.BigEndian.Uint32(p[4:]) != xid {
		return l, errors.New("a reply to another request")
	}
	l.Addr = net.IP(append([]byte(nil), p[16:20]...))
	msg := 0
	for o := p[240:]; len(o) > 0; {
		code := o[0]
		if code == optEnd {
			break
		}
		if code == 0 {
			o = o[1:]
			continue
		}
		if len(o) < 2 || len(o) < 2+int(o[1]) {
			return l, errors.New("a truncated DHCP option")
		}
		v := o[2 : 2+int(o[1])]
		switch {
		case code == optMsgType && len(v) == 1:
			msg = int(v[0])
		case code == optLeaseTime && len(v) == 4:
			if s := binary.BigEndian.Uint32(v); s != 0xffffffff {
				l.Lease = time.Duration(s) * time.Second
			}
		}
		o = o[2+len(v):]
	}
	switch msg {
	case dhcpACK:
		return l, nil
	case dhcpNAK:
		return l, errNAK
	}
	return l, fmt.Errorf("a DHCP reply of type %d, not an ACK or a NAK", msg)
}

// pnpServer is the DHCP server the kernel's client took its answer from,
// /proc/net/pnp's bootserver (its server id), or nil.
func pnpServer(pnp string) net.IP {
	for _, line := range strings.Split(pnp, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "bootserver" {
			if ip := net.ParseIP(f[1]).To4(); ip != nil && !ip.IsUnspecified() {
				return ip
			}
		}
	}
	return nil
}

// dhcpClient renews one address from one server over conn.
type dhcpClient struct {
	conn   net.PacketConn // bound to the client port, 68 in the guest
	server net.Addr       // the server's port, 67 in the guest
	addr   net.IP
	hw     net.HardwareAddr
	wait   time.Duration // for a reply, each of tries
	tries  int
}

// renew asks the server to renew the address: tries requests, each with
// a new xid, waiting wait for a reply to it.
func (c *dhcpClient) renew() (dhcpLease, error) {
	var last error = errors.New("no reply")
	buf := make([]byte, 1500)
	for i := 0; i < c.tries; i++ {
		var x [4]byte
		_, _ = rand.Read(x[:])
		xid := binary.BigEndian.Uint32(x[:])
		req, err := dhcpRenewal(xid, c.addr, c.hw)
		if err != nil {
			return dhcpLease{}, err
		}
		if _, err := c.conn.WriteTo(req, c.server); err != nil {
			last = err
			continue
		}
		deadline := time.Now().Add(c.wait)
		for {
			_ = c.conn.SetReadDeadline(deadline)
			n, _, err := c.conn.ReadFrom(buf)
			if err != nil {
				last = fmt.Errorf("no reply in %v", c.wait)
				break
			}
			l, err := parseDHCPReply(buf[:n], xid)
			if err == nil || errors.Is(err, errNAK) {
				return l, err
			}
		}
	}
	return dhcpLease{}, fmt.Errorf("%d requests: %w", c.tries, last)
}

// Bounds of keepLease's waits: never sooner than dhcpMinWait between two
// renewals, whatever a lease says, and a failed one retried after half
// what is left of the lease, at least dhcpMinWait and at most
// dhcpRetryMax. It checks the time every dhcpTick against the wall clock,
// not a timer: a guest's clocks stop while the Mac sleeps, and the clock
// keeper steps the wall clock, not the monotonic one, when it wakes.
const (
	dhcpMinWait  = 30 * time.Second
	dhcpRetryMax = 10 * time.Minute
	dhcpTick     = 15 * time.Second
)

// keepLease renews c's lease until done is closed: at once, then at half
// of each lease the server gives. What it says goes to logf: the first
// lease, a change in it, each failure, and the renewal after one.
func keepLease(c *dhcpClient, logf func(string, ...any), done <-chan struct{}, tick time.Duration) {
	now := func() time.Time { return time.Now().Round(0) } // the wall clock only
	var (
		known   time.Duration // the lease, once a server said it
		expires time.Time
		failing bool
	)
	next := now()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if !now().Before(next) {
			l, err := c.renew()
			at := now()
			switch {
			case err == nil:
				if !l.Addr.Equal(c.addr) {
					logf("DHCP: the server at %v renewed %v instead of %v, which the guest keeps", c.server, l.Addr, c.addr)
				}
				if l.Lease == 0 {
					logf("DHCP: the lease on %v is infinite: no renewals", c.addr)
					return
				}
				switch {
				case known == 0:
					logf("DHCP: lease on %v from %v for %v, MAC %v; renewing every %v", c.addr, c.server, l.Lease, c.hw, max(l.Lease/2, dhcpMinWait))
				case failing || l.Lease != known:
					logf("DHCP: renewed %v for %v", c.addr, l.Lease)
				}
				known, expires, failing = l.Lease, at.Add(l.Lease), false
				next = at.Add(max(l.Lease/2, dhcpMinWait))
			default:
				failing = true
				retry := dhcpRetryMax
				if known != 0 {
					retry = min(max(expires.Sub(at)/2, dhcpMinWait), dhcpRetryMax)
				}
				what := "the lease was never learned"
				if known != 0 {
					if left := expires.Sub(at); left > 0 {
						what = fmt.Sprintf("the lease ends in %v", left.Round(time.Second))
					} else {
						what = fmt.Sprintf("the lease ended %v ago: the server may give %v to another machine", (-left).Round(time.Second), c.addr)
					}
				}
				logf("DHCP: renewing %v failed: %v; %s; retrying in %v", c.addr, err, what, retry)
				next = at.Add(retry)
			}
		}
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}
