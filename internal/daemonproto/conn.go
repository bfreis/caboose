package daemonproto

import (
	"errors"
	"net"
	"time"
)

// Conn is a connection speaking the protocol, with a deadline per frame. It
// is for one reader and one writer at a time.
type Conn struct {
	nc net.Conn
	r  *Reader
}

// NewConn wraps nc.
func NewConn(nc net.Conn) *Conn { return &Conn{nc: nc, r: NewReader(nc)} }

// Close closes the connection.
func (c *Conn) Close() error { return c.nc.Close() }

// Raw returns the underlying connection (for a peer-credential check).
func (c *Conn) Raw() net.Conn { return c.nc }

// Read reads one frame, waiting at most d (no limit if d <= 0).
func (c *Conn) Read(d time.Duration) (*Frame, error) {
	c.setDeadline(c.nc.SetReadDeadline, d)
	return c.r.Read()
}

// Write writes one frame, waiting at most d (no limit if d <= 0).
func (c *Conn) Write(f *Frame, d time.Duration) error {
	c.setDeadline(c.nc.SetWriteDeadline, d)
	return Write(c.nc, f)
}

func (c *Conn) setDeadline(set func(time.Time) error, d time.Duration) {
	if d > 0 {
		_ = set(time.Now().Add(d))
	} else {
		_ = set(time.Time{})
	}
}

// ServerHandshake is the daemon's side of the hello. It waits at most d for
// the client's hello, answers with the daemon's own, and reports how the
// client's protocol compares. A client that does not open with a valid hello
// (a hello naming the kind daemon or web included: those are the daemon's
// own hello's and its own web page's) is refused with KindBadRequest; one
// on an older protocol gets the daemon's hello (so it can say what it met)
// and then KindProtocolMismatch, and an error is returned for both. A client on a newer protocol is let through
// with CompatClientNewer: the daemon should serve it only ping, status,
// stop and replace, and expects a replace.
func ServerHandshake(c *Conn, daemonVersion string, d time.Duration) (*Frame, Compat, error) {
	return serverHandshake(c, daemonVersion, ProtocolVersion, d)
}

// ServerHandshakeProtocol is ServerHandshake claiming protocol proto
// rather than ProtocolVersion: for tests that play a daemon of another
// protocol.
func ServerHandshakeProtocol(c *Conn, daemonVersion string, proto int, d time.Duration) (*Frame, Compat, error) {
	return serverHandshake(c, daemonVersion, proto, d)
}

func serverHandshake(c *Conn, daemonVersion string, proto int, d time.Duration) (*Frame, Compat, error) {
	hello, err := c.Read(d)
	switch {
	case err != nil:
	case hello.Type != TypeHello || hello.Client == ClientDaemon:
		err = bad("a hello from a client must come first")
	case hello.Client == ClientWeb:
		// The web kind is the daemon's own page's, which the daemon's HTTP
		// handler sets: no socket client may claim it.
		err = bad("the web kind is only the daemon's own HTTP page, never a socket client")
	}
	if err != nil {
		if errors.Is(err, ErrBadFrame) || errors.Is(err, ErrLineTooLong) {
			_ = c.Write(Failure(0, NewError(KindBadRequest, "%v", err)), d)
		}
		return nil, CompatSame, err
	}
	if err := c.Write(helloProto(ClientDaemon, daemonVersion, proto), d); err != nil {
		return nil, CompatSame, err
	}
	cmp := Compare(hello.Protocol, proto)
	if cmp == CompatClientOlder {
		e := NewError(KindProtocolMismatch, "this daemon (caboose %s) speaks protocol %d and the client protocol %d; update caboose, or run caboose daemon stop", daemonVersion, proto, hello.Protocol)
		_ = c.Write(Failure(0, e), d)
		return hello, cmp, e
	}
	return hello, cmp, nil
}

// ReadBy reads one frame, waiting until t (no limit if t is zero).
func (c *Conn) ReadBy(t time.Time) (*Frame, error) {
	_ = c.nc.SetReadDeadline(t)
	return c.r.Read()
}

// WriteBy writes one frame, waiting until t (no limit if t is zero).
func (c *Conn) WriteBy(f *Frame, t time.Time) error {
	_ = c.nc.SetWriteDeadline(t)
	return Write(c.nc, f)
}
