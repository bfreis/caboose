package webui

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// Server settings.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
	maxHeaderBytes    = 16 << 10
	// MaxConns is how many connections may be open at once; one more is
	// closed as it is taken.
	MaxConns = 64
)

// CheckAddr refuses an address the page may not be served on: anything
// but an IPv4 loopback address. Never a wildcard, never IPv6 (the page's
// origin is http://127.0.0.1:PORT, and a browser sends no other Host),
// never a name.
func CheckAddr(a netip.AddrPort) error {
	if !a.IsValid() || !a.Addr().Is4() || !a.Addr().IsLoopback() {
		return fmt.Errorf("%s is not an IPv4 loopback address: the web page is served on 127.0.0.1 only", a)
	}
	return nil
}

// Listen listens on a, which CheckAddr must take, with at most MaxConns
// connections open at once. A port in use is said so.
func Listen(a netip.AddrPort) (net.Listener, error) {
	if err := CheckAddr(a); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp4", a.String())
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("%s is in use by another process", a)
		}
		return nil, err
	}
	// What was bound, checked again: never anything but loopback.
	if got, ok := Bound(ln); !ok || CheckAddr(got) != nil {
		ln.Close()
		return nil, fmt.Errorf("listening on %s bound %v, not a loopback address", a, ln.Addr())
	}
	return &limitListener{Listener: ln, max: MaxConns}, nil
}

// Bound is the address a listener from Listen bound, and false for one
// that is not TCP.
func Bound(ln net.Listener) (netip.AddrPort, bool) {
	t, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	ap := t.AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), true
}

// Origin is the page's origin on an address: "http://127.0.0.1:7171".
func Origin(a netip.AddrPort) string { return "http://" + a.String() }

// NewServer is the page's HTTP server: plain HTTP/1.1, bounded in time and
// size, logging its own errors to errLog.
func NewServer(h http.Handler, errLog *log.Logger) *http.Server {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		Protocols:         p,
		// OPTIONS * reaches the handler, which refuses it with the
		// page's headers, instead of net/http answering it itself.
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     errLog,
	}
}

// limitListener closes a connection at once when max are open already.
type limitListener struct {
	net.Listener
	max  int64
	open atomic.Int64
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.open.Add(1) > l.max {
			l.open.Add(-1)
			c.Close()
			continue
		}
		return &limitConn{Conn: c, l: l}, nil
	}
}

type limitConn struct {
	net.Conn
	l    *limitListener
	once sync.Once
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.l.open.Add(-1) })
	return err
}

// ErrorLog is a logger for the server's own errors that writes at most a
// line a second to lg, and says how many it left out.
func ErrorLog(lg *log.Logger, now func() time.Time) *log.Logger {
	if now == nil {
		now = time.Now
	}
	return log.New(&limitedWriter{lg: lg, now: now}, "", 0)
}

type limitedWriter struct {
	lg      *log.Logger
	now     func() time.Time
	mu      sync.Mutex
	last    time.Time
	dropped int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if !w.last.IsZero() && now.Sub(w.last) < time.Second {
		w.dropped++
		return len(p), nil
	}
	w.last = now
	msg := string(p)
	if w.dropped > 0 {
		msg = fmt.Sprintf("(%d more left out) %s", w.dropped, msg)
		w.dropped = 0
	}
	if w.lg != nil {
		w.lg.Printf("web: %s", daemonproto.Text(strings.TrimSuffix(msg, "\n"), 512))
	}
	return len(p), nil
}

var _ io.Writer = (*limitedWriter)(nil)
