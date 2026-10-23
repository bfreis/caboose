package daemonproto

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Timeouts a client uses when its Options leave them zero.
const (
	DefaultDialTimeout    = 2 * time.Second
	DefaultHelloTimeout   = 2 * time.Second
	DefaultRequestTimeout = 10 * time.Second
)

// Options configure Dial.
type Options struct {
	Client  string // ClientCLI, ClientWeb or ClientApp
	Version string // this caboose's version

	DialTimeout    time.Duration // connecting
	HelloTimeout   time.Duration // the hello exchange
	RequestTimeout time.Duration // each request, unless its context ends sooner

	// Protocol is the protocol to claim; 0 is ProtocolVersion. Only tests
	// set it, to play a newer caboose.
	Protocol int
}

func (o Options) proto() int {
	if o.Protocol != 0 {
		return o.Protocol
	}
	return ProtocolVersion
}

func (o Options) withDefaults() Options {
	if o.DialTimeout <= 0 {
		o.DialTimeout = DefaultDialTimeout
	}
	if o.HelloTimeout <= 0 {
		o.HelloTimeout = DefaultHelloTimeout
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = DefaultRequestTimeout
	}
	return o
}

// Client is a connection to the daemon. Calls are serialised. Any failure of
// the transport or the framing, and any timeout, closes the connection: the
// stream's state is unknown, so later calls fail with KindUnavailable.
type Client struct {
	opts   Options
	mu     sync.Mutex
	conn   *Conn
	nextID uint64
	broken bool
	daemon Frame
}

func deadlineOf(ctx context.Context) time.Time {
	d, _ := ctx.Deadline()
	return d
}

// CheckSocketPath refuses a socket path too long for a Unix socket, saying
// what to change.
func CheckSocketPath(path string) error {
	if len(path) > MaxSocketPath {
		return NewError(KindBadRequest, "the daemon's socket path %q is %d bytes, over the %d a Unix socket allows; use a shorter CABOOSE_HOME", path, len(path), MaxSocketPath)
	}
	return nil
}

// Dial connects to the daemon's socket and exchanges hellos. Failures are
// *Error: KindUnavailable when nothing answers (no socket, refused, a stale
// socket), KindTimeout when the daemon is there but silent, KindBadFrame for
// a peer that does not speak the protocol, KindProtocolMismatch when this
// client's protocol is older than the daemon's (the message says what to
// run). When this client's protocol is newer the dial succeeds and
// Compat reports CompatClientNewer: the caller should Replace the daemon with
// force, and start another.
func Dial(ctx context.Context, socket string, opts Options) (*Client, error) {
	opts = opts.withDefaults()
	if err := CheckSocketPath(socket); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: opts.DialTimeout}
	nc, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, transportError(err, "connecting to the daemon at "+socket)
	}
	c := &Client{opts: opts, conn: NewConn(nc)}
	if err := c.handshake(ctx); err != nil {
		c.conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) handshake(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.HelloTimeout)
	defer cancel()
	by := deadlineOf(ctx)
	stop := context.AfterFunc(ctx, func() { _ = c.conn.Raw().SetDeadline(time.Now()) })
	defer stop()
	if err := c.conn.WriteBy(helloProto(c.opts.Client, c.opts.Version, c.opts.proto()), by); err != nil {
		return c.fail(ctx, err, "sending hello")
	}
	f, err := c.conn.ReadBy(by)
	if err != nil {
		return c.fail(ctx, err, "waiting for the daemon's hello")
	}
	if f.Type == TypeResponse && f.Error != nil {
		return f.Error
	}
	if f.Type != TypeHello || f.Client != ClientDaemon {
		return NewError(KindBadFrame, "the peer did not answer with a daemon hello")
	}
	c.daemon = *f
	if Compare(c.opts.proto(), f.Protocol) == CompatClientOlder {
		return NewError(KindProtocolMismatch, "the running daemon (caboose %s) speaks protocol %d, newer than this caboose's %d; update caboose, or run caboose daemon stop", f.Version, f.Protocol, c.opts.proto())
	}
	return nil
}

// Daemon returns the daemon's hello: its protocol and caboose version.
func (c *Client) Daemon() Frame { return c.daemon }

// Compat reports how this client's protocol relates to the daemon's.
func (c *Client) Compat() Compat { return Compare(c.opts.proto(), c.daemon.Protocol) }

// Close closes the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.broken = true
	return c.conn.Close()
}

// Call sends a request and decodes its result into result (nil to ignore
// it). A daemon's error comes back as the *Error it sent. The call ends when
// ctx does or after Options.RequestTimeout, whichever is first.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken {
		return NewError(KindUnavailable, "the connection to the daemon is closed")
	}
	c.nextID++
	req, err := Request(c.nextID, method, params)
	if err == nil {
		err = req.Validate()
	}
	if err != nil {
		return NewError(KindBadRequest, "%v", err) // nothing sent: the connection stays usable
	}
	ctx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
	defer cancel()
	// One absolute deadline for the whole call: a peer cannot stretch it
	// by sending frames.
	by := deadlineOf(ctx)
	stop := context.AfterFunc(ctx, func() { _ = c.conn.Raw().SetDeadline(time.Now()) })
	defer stop()

	if err := c.conn.WriteBy(req, by); err != nil {
		return c.fail(ctx, err, "sending "+method)
	}
	f, err := c.conn.ReadBy(by)
	if err != nil {
		return c.fail(ctx, err, "waiting for the answer to "+method)
	}
	if err := ctx.Err(); err != nil {
		return c.fail(ctx, err, "waiting for the answer to "+method)
	}
	switch {
	case f.Type != TypeResponse || (f.ID != req.ID && f.ID != 0):
		return c.poison(NewError(KindBadFrame, "unexpected %s (id %d) while waiting for the answer to %s", f.Type, f.ID, method))
	case f.Error != nil:
		if f.ID == 0 {
			c.poison(nil)
		}
		return f.Error
	}
	if result != nil {
		if err := unmarshal(f.Result, result); err != nil {
			return c.poison(NewError(KindBadFrame, "the result of %s: %v", method, err))
		}
		if v, ok := result.(interface{ Validate() error }); ok {
			if err := v.Validate(); err != nil {
				return c.poison(NewError(KindBadFrame, "the result of %s: %v", method, err))
			}
		}
	}
	return nil
}

func (c *Client) poison(e *Error) error {
	c.broken = true
	c.conn.Close()
	if e == nil {
		return nil
	}
	return e
}

// fail turns a transport or framing error into an *Error and, as the stream
// can no longer be trusted, closes the connection.
func (c *Client) fail(ctx context.Context, err error, doing string) error {
	c.broken = true
	c.conn.Close()
	if ctx.Err() != nil && !errors.Is(err, ErrBadFrame) && !errors.Is(err, ErrLineTooLong) {
		if errors.Is(ctx.Err(), context.Canceled) {
			return NewError(KindUnavailable, "%s: canceled", doing)
		}
		return NewError(KindTimeout, "the daemon did not answer while %s; run caboose daemon restart", doing)
	}
	if errors.Is(err, ErrBadFrame) || errors.Is(err, ErrLineTooLong) {
		return NewError(KindBadFrame, "%s: %v", doing, err)
	}
	return transportError(err, doing)
}

func transportError(err error, doing string) *Error {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, os.ErrDeadlineExceeded):
		return NewError(KindTimeout, "%s: timed out; run caboose daemon restart", doing)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return NewError(KindUnavailable, "%s: the daemon closed the connection", doing)
	default:
		return NewError(KindUnavailable, "%s: %v", doing, err)
	}
}

// Ping checks the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	return c.Call(ctx, MethodPing, nil, nil)
}

// Status returns the daemon's and every environment's state.
func (c *Client) Status(ctx context.Context) (*StatusResult, error) {
	var r StatusResult
	if err := c.Call(ctx, MethodStatus, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) envCall(ctx context.Context, method, env string) (*LinkResult, error) {
	if !ValidEnv(env) {
		return nil, NewError(KindBadRequest, "bad environment name %q", env)
	}
	var r LinkResult
	if err := c.Call(ctx, method, &EnvParams{Env: env}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// EnsureLink starts env's link if its sandbox runs and none serves it, or
// replaces one whose settings differ.
func (c *Client) EnsureLink(ctx context.Context, env string) (*LinkResult, error) {
	return c.envCall(ctx, MethodEnsureLink, env)
}

// RestartLink stops env's link and starts another.
func (c *Client) RestartLink(ctx context.Context, env string) (*LinkResult, error) {
	return c.envCall(ctx, MethodRestartLink, env)
}

// StopLink stops env's link.
func (c *Client) StopLink(ctx context.Context, env string) (*LinkResult, error) {
	return c.envCall(ctx, MethodStopLink, env)
}

// Stop asks the daemon to stop, its links and the host-exec commands with it.
func (c *Client) Stop(ctx context.Context) error {
	return c.Call(ctx, MethodStop, nil, nil)
}

// Replace asks the daemon to exit so that a newer one can start.
func (c *Client) Replace(ctx context.Context, version string, force bool) (*ReplaceResult, error) {
	var r ReplaceResult
	if err := c.Call(ctx, MethodReplace, &ReplaceParams{Version: version, Force: force}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
