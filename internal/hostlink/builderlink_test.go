package hostlink

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agent"
	"github.com/bfreis/caboose/internal/agentproto"
)

// unixPair is two ends of a Unix stream socket with small buffers: a
// link's transport, where a side that stops reading soon stops the other.
func unixPair(t *testing.T, buf int) (net.Conn, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	var conns [2]net.Conn
	for i, fd := range fds {
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, buf)
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, buf)
		f := os.NewFile(uintptr(fd), "socketpair")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c
	}
	t.Cleanup(func() { conns[0].Close(); conns[1].Close() })
	return conns[0], conns[1]
}

// sizedServer answers each connection's first line, a byte count, with
// that many bytes, then waits for the client to close.
func sizedServer(t *testing.T) string {
	t.Helper()
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
				var n int64
				if _, err := fmt.Fscanf(bufio.NewReader(c), "%d\n", &n); err != nil {
					return
				}
				buf := make([]byte, 32<<10)
				for n > 0 {
					k := min(n, int64(len(buf)))
					if _, err := c.Write(buf[:k]); err != nil {
						return
					}
					n -= k
				}
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// freeLoopback is a loopback address and port nothing listens on.
func freeLoopback(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// proxyGet asks the proxy at proxy for n bytes from the server, through a
// CONNECT, and returns the connection once its first byte came, with what
// is left to read.
func proxyGet(proxy string, n int64) (net.Conn, *bufio.Reader, error) {
	c, err := net.DialTimeout("tcp", proxy, 5*time.Second)
	if err != nil {
		return nil, nil, err
	}
	if _, err := fmt.Fprintf(c, "CONNECT files.example:443 HTTP/1.1\r\nHost: files.example:443\r\n\r\n"); err != nil {
		c.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return nil, nil, fmt.Errorf("CONNECT: %s", resp.Status)
	}
	if _, err := fmt.Fprintf(c, "%d\n", n); err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, br, nil
}

// builderLinked runs the builder's link as builderLink does -- a real
// agent's RunLink at one end of a socket, hostlink.Run with the outbound
// proxy alone at the other -- and returns where the agent serves the
// proxy.
func builderLinked(t *testing.T, buf int, g *gate) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the agent reads /proc")
	}
	srv := sizedServer(t)
	listen := freeLoopback(t)
	var hc, ac net.Conn
	if g == nil {
		hc, ac = unixPair(t, buf)
	} else {
		// The hypervisor between them, which the test can freeze.
		var hr, ar net.Conn
		hc, hr = unixPair(t, buf)
		ar, ac = unixPair(t, buf)
		go g.copy(ar, hr)
		go g.copy(hr, ar)
	}
	dir := t.TempDir()
	go agent.RunLink(ac, ac, agent.Config{Socket: dir + "/s", ProcRoot: "/proc", Interval: 20 * time.Millisecond})
	sess := agentproto.NewSession(hc, hc, true)
	t.Cleanup(func() { sess.Close() })
	ports, _ := ParsePortsOf("egress_ports", "443")
	var d net.Dialer
	eg := &Egress{
		Listen: listen, Ports: ports,
		Resolver: &fakeNet{names: map[string][]netip.Addr{"files.example": {netip.MustParseAddr("140.82.112.9")}}},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, srv)
		},
		Own: func() ([]netip.Addr, error) { return nil, nil },
	}
	cfg := Config{Ports: PortSet{}, OpenURL: OpenOff, Actions: &fakeActions{}, Log: log.New(io.Discard, "", 0), Egress: eg}
	go Run(sess, cfg)
	if err := agent.WaitProxy(listen, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	return listen
}

// A build's traffic through the builder's link: a long download, idle
// connections held open as dockerd keeps them, and new connections made
// while the download runs. Every one of them gets through, and the
// download does not crawl.
func TestBuilderLinkDownloadAndNewConnections(t *testing.T) {
	proxy := builderLinked(t, 16<<10, nil)

	// dockerd's: opened, used a little, then left open.
	for i := 0; i < 3; i++ {
		c, br, err := proxyGet(proxy, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(io.Discard, br, 1000); err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}

	const big = 200 << 20
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		c, br, err := proxyGet(proxy, big)
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		_, err = io.CopyN(io.Discard, br, big)
		done <- err
	}()

	// New connections while it runs, each answered promptly.
	var wg sync.WaitGroup
	slow := make(chan string, 64)
	for i := 0; i < 20; i++ {
		time.Sleep(20 * time.Millisecond)
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			c, br, err := proxyGet(proxy, 64<<10)
			if err != nil {
				slow <- err.Error()
				return
			}
			defer c.Close()
			if _, err := io.CopyN(io.Discard, br, 64<<10); err != nil {
				slow <- err.Error()
				return
			}
			if d := time.Since(t0); d > 3*time.Second {
				slow <- fmt.Sprintf("a new connection took %v", d)
			}
		}()
	}
	wg.Wait()
	close(slow)
	for s := range slow {
		t.Error(s)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the download stalled")
	}
	t.Logf("%d MiB in %v", big>>20, time.Since(start).Round(time.Millisecond))
}

// gate is a transport that stops moving bytes, both ways, while shut.
type gate struct {
	mu   sync.Mutex
	cond *sync.Cond
	shut bool
}

func newGate() *gate {
	g := &gate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gate) set(shut bool) {
	g.mu.Lock()
	g.shut = shut
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *gate) wait() {
	g.mu.Lock()
	for g.shut {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

func (g *gate) copy(dst io.Writer, src io.Reader) {
	buf := make([]byte, 4096)
	for {
		g.wait()
		n, err := src.Read(buf)
		if n > 0 {
			g.wait()
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// The transport freezing both ways mid-download, while new connections
// ask for the host, stops nothing for good: once it moves again, the
// download finishes and every connection is served.
func TestBuilderLinkRecoversFromAFrozenTransport(t *testing.T) {
	g := newGate()
	proxy := builderLinked(t, 16<<10, g)
	const big = 64 << 20
	done := make(chan error, 1)
	go func() {
		c, br, err := proxyGet(proxy, big)
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		_, err = io.CopyN(io.Discard, br, big)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	g.set(true)
	var served atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, br, err := proxyGet(proxy, 1000)
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			if _, err := io.CopyN(io.Discard, br, 1000); err != nil {
				t.Error(err)
				return
			}
			served.Add(1)
		}()
	}
	time.Sleep(3 * time.Second)
	g.set(false)
	wg.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the download never finished")
	}
	if n := served.Load(); n != 20 {
		t.Fatalf("%d of 20 connections served", n)
	}
}
