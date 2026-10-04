package agent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bfreis/caboose/internal/agentproto"
)

// The outbound proxy: under vm, the host's hello asks the agent to serve
// an HTTP proxy on the guest's loopback (Egress), and HTTP_PROXY points
// there. Each client is a request to the host (OpConnect), which resolves
// the name, checks it and dials it from the host, where a VPN's routes
// apply; the agent only parses HTTP, and never resolves or dials anything.
// It takes CONNECT host:port, for HTTPS and any other TCP (ssh's
// ProxyCommand, `caboose-agent connect`), and a plain-HTTP request for an
// absolute http:// URL, which it sends on in origin form, keeping both the
// client's connection and its stream to the host from one request to the
// next while they are for the same host and port, and sending on those the
// client sends before their answers without waiting for them either. A
// refusal is answered in HTTP, which curl and git print.

const (
	// egressHeadLimit bounds a request's line and headers.
	egressHeadLimit = 32 << 10
	// egressWait bounds the host's answer, and then its stream: above the
	// host's own 10 seconds to connect.
	egressWait = 20 * time.Second
	// maxEgressClients bounds the proxy's connections at once, those
	// still sending their request included; the host bounds those it
	// dials at 128.
	maxEgressClients = 512
)

// egressHeadTimeout bounds the wait for a request's line and headers, so
// a client that sends them a byte at a time is cut off. A variable for the
// tests.
var egressHeadTimeout = 30 * time.Second

// egressIdleTimeout bounds the wait for a kept connection's next request.
// A variable for the tests.
var egressIdleTimeout = 90 * time.Second

// egressBodyGrace is how long a request's body may take to be sent once
// its response is over before the connection is given up.
const egressBodyGrace = time.Second

// serveEgress serves the outbound proxy at addr, as the host's hello asks,
// until the link ends: once per link. Only a loopback address is taken.
// An address in use, as by a link that has not yet gone, is tried again
// every second.
func (l *Link) serveEgress(addr string) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().IsLoopback() || ap.Port() == 0 {
		fmt.Fprintf(os.Stderr, "caboose-agent: not serving the outbound proxy at %q, which is no loopback address and port\n", addr)
		return
	}
	l.mu.Lock()
	serving := l.egressServing
	l.egressServing = true
	l.egressPort = int(ap.Port())
	l.mu.Unlock()
	if serving {
		return
	}
	go func() {
		var ln net.Listener
		for logged := false; ; logged = true {
			if ln, err = net.Listen("tcp", addr); err == nil {
				break
			}
			if !logged {
				fmt.Fprintf(os.Stderr, "caboose-agent: cannot serve the outbound proxy at %s, trying again every second: %v\n", addr, err)
			}
			select {
			case <-l.sess.Done():
				return
			case <-time.After(time.Second):
			}
		}
		go func() {
			<-l.sess.Done()
			ln.Close()
		}()
		l.acceptEgress(ln)
	}()
}

func (l *Link) acceptEgress(ln net.Listener) {
	slots := make(chan struct{}, maxEgressClients)
	idle := egressIdleTimeout
	for {
		c, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil { // out of files, say: not the proxy's end
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				l.proxyClient(c, idle)
			}()
		default:
			go func() {
				httpError(c, http.StatusServiceUnavailable, "too many connections to caboose's outbound proxy; try again shortly")
				c.Close()
			}()
		}
	}
}

// proxyClient serves one connection to the proxy: a CONNECT and its
// tunnel, or plain-HTTP requests, one after another while both the client
// and the server keep the connection, each sent on the same stream to the
// host for as long as they are for the same host and port.
func (l *Link) proxyClient(c net.Conn, idle time.Duration) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		c.Close()
		return
	}
	// A client is not to wait for the host while the proxy dies with the
	// link: closing it ends whatever this connection is doing.
	stop := closeOnDone(l.sess.Done(), tc)
	defer stop()
	defer tc.Close()
	lr := &io.LimitedReader{R: tc}
	br := bufio.NewReader(lr)
	client := &bufferedConn{tc: tc, r: br}
	p := &pipeline{l: l, c: client, idle: idle}
	p.cond = sync.NewCond(&p.mu)
	defer p.close()
	// What the client is told is wrong goes after the responses to what
	// it sent before.
	refuse := func(status int, msg string) {
		if p.drain() {
			httpError(tc, status, msg)
			p.end()
		}
	}
	for first := true; ; first = false {
		if !first && !p.awaitRequest(br) {
			return
		}
		req, status, msg := readProxyRequest(tc, lr, br)
		if req == nil {
			if status != 0 {
				refuse(status, msg)
			}
			return
		}
		switch {
		case req.Method == http.MethodConnect:
			host, port, err := splitTarget(req.URL.Host, -1)
			if err != nil {
				refuse(http.StatusBadRequest, "CONNECT needs host:port: "+err.Error())
				return
			}
			if !p.drain() {
				return
			}
			p.closeUpstream()
			l.tunnel(client, host, port)
			return
		case req.URL.IsAbs() && req.URL.Scheme == "http":
			host, port, err := splitTarget(req.URL.Host, 80)
			if err != nil {
				refuse(http.StatusBadRequest, "the URL's host: "+err.Error())
				return
			}
			if !p.forward(req, host, port) {
				return
			}
		case req.URL.IsAbs():
			refuse(http.StatusBadRequest, fmt.Sprintf("a %s:// URL goes through CONNECT, not as a request to the proxy", req.URL.Scheme))
			return
		default:
			refuse(http.StatusBadRequest, "this is caboose's outbound proxy (HTTP_PROXY, HTTPS_PROXY): it takes CONNECT host:port, or a request for an absolute http:// URL")
			return
		}
	}
}

// readProxyRequest reads a request's line and headers within
// egressHeadTimeout and egressHeadLimit bytes, or says what to answer the
// client when it cannot: nothing, when status is 0. Its body is then read
// without either limit.
func readProxyRequest(tc *net.TCPConn, lr *io.LimitedReader, br *bufio.Reader) (req *http.Request, status int, msg string) {
	deadline := time.Now().Add(egressHeadTimeout)
	_ = tc.SetReadDeadline(deadline)
	lr.N = egressHeadLimit
	req, err := http.ReadRequest(br)
	if err != nil {
		switch {
		case lr.N <= 0:
			return nil, http.StatusRequestHeaderFieldsTooLarge, fmt.Sprintf("the request's headers are over %d bytes", egressHeadLimit)
		case errors.Is(err, os.ErrDeadlineExceeded) || !time.Now().Before(deadline):
			// Not always the error: a line cut short by the deadline
			// can read as a malformed one.
			return nil, http.StatusRequestTimeout, fmt.Sprintf("no complete request in %v", egressHeadTimeout)
		case errors.Is(err, io.EOF):
			// Closed before a request: nothing to answer.
			return nil, 0, ""
		default:
			return nil, http.StatusBadRequest, "not an HTTP request: " + err.Error()
		}
	}
	_ = tc.SetReadDeadline(time.Time{})
	lr.N = math.MaxInt64
	return req, 0, ""
}

// closeOnDone closes c when done is: a link that goes away takes its
// clients with it. The returned func stops the watch.
func closeOnDone(done <-chan struct{}, c io.Closer) func() {
	stop := make(chan struct{})
	go func() {
		select {
		case <-done:
			c.Close()
		case <-stop:
		}
	}()
	return func() { close(stop) }
}

// splitTarget reads host:port, or host alone when defPort is one. The
// host goes to the host bare: an IPv6 address without its brackets.
func splitTarget(hostport string, defPort int) (string, int, error) {
	host, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		// No port: fine where there is a default, unless the colon is a
		// bare IPv6 address's, which must be in brackets.
		if defPort < 0 || (strings.Contains(hostport, ":") && !strings.HasPrefix(hostport, "[")) {
			return "", 0, fmt.Errorf("%q is not host:port", printable(hostport))
		}
		host, ps = strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]"), strconv.Itoa(defPort)
	}
	if ps == "" && defPort > 0 {
		ps = strconv.Itoa(defPort)
	}
	port, err := strconv.Atoi(ps)
	if err != nil || !agentproto.ValidPort(port) {
		return "", 0, fmt.Errorf("%q is not a port", printable(ps))
	}
	if host == "" || len(host) > agentproto.MaxEgressHost {
		return "", 0, fmt.Errorf("the host must be a name or an address, of at most %d bytes", agentproto.MaxEgressHost)
	}
	return host, port, nil
}

// dialHost asks the host for a connection to host:port, and answers the
// client in HTTP when there is none.
func (l *Link) dialHost(c net.Conn, host string, port int) *agentproto.Stream {
	st, r := l.openStream(agentproto.Message{Op: agentproto.OpConnect, Host: host, Port: port}, egressWait)
	if st == nil {
		httpError(c, egressStatus(r.Reason), r.Error)
	}
	return st
}

// egressStatus is the HTTP status that answers a refusal of reason.
func egressStatus(reason string) int {
	switch reason {
	case agentproto.EgressOff, agentproto.EgressBad, agentproto.EgressPort, agentproto.EgressDenied:
		return http.StatusForbidden
	case agentproto.EgressResolve, agentproto.EgressDial:
		return http.StatusBadGateway
	default: // EgressLimit, and the link's own trouble: try again later
		return http.StatusServiceUnavailable
	}
}

// tunnel connects the client to host:port, after a 200.
func (l *Link) tunnel(c *bufferedConn, host string, port int) {
	st := l.dialHost(c.tc, host, port)
	if st == nil {
		return
	}
	_ = c.tc.SetWriteDeadline(time.Now().Add(egressHeadTimeout))
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		st.Close()
		return
	}
	_ = c.tc.SetWriteDeadline(time.Time{})
	agentproto.Splice(st, c)
}

// hopByHop are the headers that concern the connection to the proxy, not
// the request.
var hopByHop = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// upstream is a client connection's stream to the host for plain HTTP,
// kept from one request to the next while they are for the same host and
// port and the server keeps the connection.
type upstream struct {
	st       *agentproto.Stream
	br       *bufio.Reader
	host     string
	port     int
	target   string
	read     int64 // bytes read from st
	answered int   // responses read whole from st
}

func newUpstream(st *agentproto.Stream, host string, port int) *upstream {
	u := &upstream{st: st, host: host, port: port, target: net.JoinHostPort(host, strconv.Itoa(port))}
	u.br = bufio.NewReader(readCounter{st, &u.read})
	return u
}

type readCounter struct {
	r io.Reader
	n *int64
}

func (c readCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

// pipeline is a client connection's plain-HTTP requests on their way: the
// client's reader sends each on the upstream, and respond sends their
// responses back, in order. A request goes while others are still being
// answered only when it could go again on a new stream, to the same host
// and port: no body, no Expect, and at most egressPipelineDepth in all.
// Any other waits for them all to be answered, and goes alone.
type pipeline struct {
	l    *Link
	c    *bufferedConn
	idle time.Duration

	// wmu is held while a request is written, and by respond while it
	// sends again on a new stream what one the server closed left
	// unanswered.
	wmu sync.Mutex

	mu      sync.Mutex
	cond    *sync.Cond
	up      *upstream
	queue   []*inflight // sent and not yet answered whole, oldest first
	waiting bool        // the reader waits for the client's next request
	ended   bool        // the client's connection is over
	stopped bool
	done    chan struct{} // closed when respond returns; nil before it starts
}

// inflight is a request sent on the upstream and not yet answered.
type inflight struct {
	req     *http.Request
	up      *upstream
	retry   bool       // no body: it can go again on a new stream
	alone   bool       // a body or an Expect: nothing goes beside it
	written chan error // with a body, its writer's end
}

// egressPipelineDepth bounds the requests a client's connection has on
// its upstream at once, the one being answered included: apt sends 10.
const egressPipelineDepth = 10

// egressLinger is how long a client connection the proxy ends after a
// response is kept to read what the client still sends, so that the close
// does not reset it before the client has read the response.
const egressLinger = time.Second

// forward sends req on to host:port in origin form, on the upstream when
// it is to the same place and opening one when not, beside the requests
// still being answered when it can. It says whether the client's
// connection goes on to another request: not when the client asked to
// close, or anything failed.
func (p *pipeline) forward(req *http.Request, host string, port int) bool {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	keep := !req.Close
	stripHopByHop(req.Header)
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header["User-Agent"] = []string{""} // Write adds none for an empty one
	}
	req.Close = !keep
	// A request with no body, of a method that asks nothing of the server
	// but an answer, can go again on a new stream when a kept stream
	// turns out closed by the server before it answered, as net/http's
	// client does.
	e := &inflight{req: req, retry: (req.Body == nil || req.Body == http.NoBody) && replayable[req.Method]}
	e.alone = !e.retry || req.Header.Get("Expect") != ""
	if !e.retry {
		e.written = make(chan error, 1)
	}
	p.mu.Lock()
	for !p.ended && len(p.queue) > 0 && (e.alone || p.queue[0].alone ||
		p.up.target != target || len(p.queue) >= egressPipelineDepth) {
		p.cond.Wait()
	}
	if p.ended {
		p.mu.Unlock()
		return false
	}
	// Nothing in flight when another place is asked for: respond is
	// done with the upstream.
	var old *upstream
	if p.up != nil && p.up.target != target {
		old, p.up = p.up, nil
	}
	up := p.up
	p.mu.Unlock()
	if old != nil {
		old.st.Close()
	}
	if up == nil {
		st := p.l.dialHost(p.c.tc, host, port)
		if st == nil {
			p.end()
			return false
		}
		p.mu.Lock()
		p.up = newUpstream(st, host, port)
		p.mu.Unlock()
	}
	p.start()
	p.wmu.Lock()
	p.mu.Lock()
	if p.ended {
		p.mu.Unlock()
		p.wmu.Unlock()
		return false
	}
	// The upstream respond moved the requests to, when it had to.
	up = p.up
	e.up = up
	p.queue = append(p.queue, e)
	p.cond.Broadcast()
	p.mu.Unlock()
	err := req.Write(up.st)
	p.wmu.Unlock()
	if err != nil && !e.retry {
		up.st.Close() // the response is not to wait for a request cut short
	}
	if e.written != nil {
		e.written <- err
	}
	// One that can go again and did not go whole is respond's to send
	// again or give up on, as it finds the stream; after a close, the
	// client is to send nothing more.
	return keep && (err == nil || e.retry)
}

// start starts respond, once.
func (p *pipeline) start() {
	if p.done == nil {
		p.done = make(chan struct{})
		go p.respond()
	}
}

// respond answers the requests in flight, oldest first, until the
// client's connection ends or the reader stops.
func (p *pipeline) respond() {
	defer close(p.done)
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.stopped && !p.ended {
			p.cond.Wait()
		}
		if len(p.queue) == 0 || p.ended {
			p.mu.Unlock()
			return
		}
		e := p.queue[0]
		p.mu.Unlock()
		if !p.answer(e) {
			return
		}
	}
}

// answer sends e's response to the client, and says whether the
// connection goes on.
func (p *pipeline) answer(e *inflight) bool {
	p.mu.Lock()
	up := e.up
	p.mu.Unlock()
	// What of the stream came before e's response: past it, a byte read
	// is e's.
	before := up.read - int64(up.br.Buffered())
	got := false
	for {
		resp, err := http.ReadResponse(up.br, e.req)
		if err != nil {
			if !got && up.read == before && e.retry && up.answered > 0 {
				// Not a byte: a kept stream the server had closed. What
				// went after e has no body either, and all of it goes
				// again on a new stream.
				if up = p.again(up); up == nil {
					return false
				}
				before = 0
				continue
			}
			if !got {
				httpError(p.c.tc, http.StatusBadGateway, "no HTTP response from "+printable(up.target)+": "+err.Error())
				p.end()
				return false
			}
			return p.fail()
		}
		if resp.StatusCode == http.StatusSwitchingProtocols {
			// Upgrade is not sent on, so none was asked for.
			httpError(p.c.tc, http.StatusBadGateway, "the server switched protocols, which the proxy does not do")
			p.end()
			return false
		}
		if resp.StatusCode < 200 {
			// 100 Continue, 103 Early Hints: on to the client, then the
			// response itself.
			stripHopByHop(resp.Header)
			var b strings.Builder
			fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", resp.StatusCode, http.StatusText(resp.StatusCode))
			_ = resp.Header.Write(&b)
			b.WriteString("\r\n")
			if _, err := io.WriteString(p.c, b.String()); err != nil {
				return p.fail()
			}
			got = true
			continue
		}
		return p.final(e, up, resp)
	}
}

// final sends resp, e's response, to the client, and says whether the
// connection goes on.
func (p *pipeline) final(e *inflight, up *upstream, resp *http.Response) bool {
	req := e.req
	serverKeeps := !resp.Close
	stripHopByHop(resp.Header)
	// The proxy speaks HTTP/1.1 to the client, whatever the server
	// spoke; to an HTTP/1.0 client, without chunks, to the close.
	resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1
	if !req.ProtoAtLeast(1, 1) {
		resp.TransferEncoding = nil
	}
	// A response with no length of its own ends with the connection,
	// as Write says to the client (a HEAD's included).
	resp.Close = req.Close || !serverKeeps ||
		resp.ContentLength < 0 && !slices.Contains(resp.TransferEncoding, "chunked")
	if !resp.Close && !req.ProtoAtLeast(1, 1) {
		resp.Header.Set("Connection", "keep-alive") // or 1.0 closes
	}
	if err := resp.Write(p.c); err != nil {
		return p.fail()
	}
	end := resp.Close
	if e.written != nil {
		// A body still being sent once the response is over is one the
		// server refused, or a client's waiting on a 100 Continue that
		// never came: neither connection is where a next request would
		// start.
		select {
		case err := <-e.written:
			end = end || err != nil
		case <-time.After(egressBodyGrace):
			return p.fail()
		}
	}
	up.answered++
	p.mu.Lock()
	p.queue = p.queue[1:]
	// Both go on when the request went whole, and nothing came past the
	// response's end that no request asked for.
	if end = end || len(p.queue) == 0 && up.br.Buffered() > 0; end {
		p.ended = true
	} else if p.waiting && len(p.queue) == 0 {
		p.armIdle()
	}
	p.cond.Broadcast()
	p.mu.Unlock()
	if end {
		// What the client sent after is dropped: it sends it again, as
		// HTTP/1.1 has it do for a connection closed with requests
		// unanswered.
		p.end()
		return false
	}
	return true
}

// again sends the requests in flight, all of them ones that can go again,
// on a new stream to where old went, which the server closed before it
// answered them, and says what it is: nil when there is none, and the
// client's connection is over. A write that fails stops it: what that
// stream answers comes back, and what it does not goes again once more.
func (p *pipeline) again(old *upstream) *upstream {
	old.st.Close() // a write on it is to fail, not to wait
	p.wmu.Lock()
	defer p.wmu.Unlock()
	st := p.l.dialHost(p.c.tc, old.host, old.port)
	if st == nil {
		p.end()
		return nil
	}
	up := newUpstream(st, old.host, old.port)
	p.mu.Lock()
	queue := slices.Clone(p.queue)
	for _, e := range queue {
		e.up = up
	}
	p.up = up
	p.mu.Unlock()
	for _, e := range queue {
		if !e.retry {
			p.fail()
			return nil
		}
		if e.req.Write(st) != nil {
			break
		}
	}
	return up
}

// replayable are the methods of a request that can be sent twice, as when
// it goes again on a new stream: those that ask the server for nothing but
// an answer.
var replayable = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodOptions: true, http.MethodTrace: true,
}

// end ends the client's connection after what it was just sent, and the
// upstream's: its write side at once, and the rest once the client has
// had egressLinger to read it, or closed it.
func (p *pipeline) end() {
	p.mu.Lock()
	p.ended = true
	p.cond.Broadcast()
	up := p.up
	p.mu.Unlock()
	if up != nil {
		up.st.Close()
	}
	_ = p.c.tc.CloseWrite()
	time.AfterFunc(egressLinger, func() { p.c.tc.Close() })
}

// fail ends the client's connection at once, and the upstream's: the
// reader may be sending a body that is not coming.
func (p *pipeline) fail() bool {
	p.mu.Lock()
	p.ended = true
	p.cond.Broadcast()
	up := p.up
	p.mu.Unlock()
	if up != nil {
		up.st.Close()
	}
	p.c.tc.Close()
	return false
}

// awaitRequest waits for the client's next request to start, and says
// whether there is one. Kept alive, the client has idle to start it once
// everything before is answered, and the connection ends then without a
// word.
func (p *pipeline) awaitRequest(br *bufio.Reader) bool {
	p.mu.Lock()
	if p.ended {
		p.mu.Unlock()
		return false
	}
	p.waiting = true
	p.armIdle()
	p.mu.Unlock()
	_, err := br.Peek(1)
	p.mu.Lock()
	p.waiting = false
	ended := p.ended
	p.mu.Unlock()
	if err != nil && !errors.Is(err, io.EOF) && !ended {
		// Gone, or idle too long: not to wait for what is in flight. An
		// EOF may be a client that sent all it had and still reads.
		p.fail()
	}
	return err == nil && !ended
}

// armIdle sets the wait for the client's next request: idle when nothing
// is in flight, and none while a response is still to come. Under mu.
func (p *pipeline) armIdle() {
	if len(p.queue) == 0 {
		_ = p.c.tc.SetReadDeadline(time.Now().Add(p.idle))
	} else {
		_ = p.c.tc.SetReadDeadline(time.Time{})
	}
}

// drain waits for every request in flight to be answered, and says
// whether the client's connection is still there for what comes next.
func (p *pipeline) drain() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.queue) > 0 && !p.ended {
		p.cond.Wait()
	}
	return !p.ended
}

// closeUpstream closes the upstream, with nothing in flight.
func (p *pipeline) closeUpstream() {
	p.mu.Lock()
	up := p.up
	p.up = nil
	p.mu.Unlock()
	if up != nil {
		up.st.Close()
	}
}

// close ends the pipeline once what is in flight is answered: respond, and
// the upstream. A client's connection ended after a response is read
// until the client closes it, or egressLinger has closed it.
func (p *pipeline) close() {
	ended := !p.drain()
	p.mu.Lock()
	p.stopped = true
	p.cond.Broadcast()
	p.mu.Unlock()
	if p.done != nil {
		<-p.done
	}
	p.closeUpstream()
	if ended {
		_, _ = io.Copy(io.Discard, p.c.tc)
	}
}

// stripHopByHop drops from h what concerns one connection, not the
// message: hopByHop and whatever Connection names.
func stripHopByHop(h http.Header) {
	for _, f := range h["Connection"] {
		for _, name := range strings.Split(f, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHop {
		h.Del(name)
	}
}

// bufferedConn is a client's connection whose reads start with what the
// request's reader took past the request's head. It does not embed the
// connection: io.Copy would take its WriteTo, and skip what is buffered.
type bufferedConn struct {
	tc *net.TCPConn
	r  io.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error)  { return b.r.Read(p) }
func (b *bufferedConn) Write(p []byte) (int, error) { return b.tc.Write(p) }
func (b *bufferedConn) Close() error                { return b.tc.Close() }
func (b *bufferedConn) CloseWrite() error           { return b.tc.CloseWrite() }

// httpError answers the client with status and msg, then the connection
// ends.
func httpError(c net.Conn, status int, msg string) {
	body := "caboose: " + msg + "\n"
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

// printable cuts s short and drops what a terminal would act on.
func printable(s string) string {
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
}

// noProxyHost is env with the guest's hostname added to NO_PROXY and
// no_proxy, where env sets them (agentproto.EgressEnv): a server of the
// guest's own, by its name, is no host's to reach. A hostname that is no
// plain name, or one already there, is left out.
func noProxyHost(env []string, hostname string) []string {
	if hostname == "" || strings.ContainsAny(hostname, ", \t\n=") {
		return env
	}
	out := make([]string, len(env))
	for i, e := range env {
		out[i] = e
		k, v, ok := strings.Cut(e, "=")
		if !ok || k != "NO_PROXY" && k != "no_proxy" {
			continue
		}
		if !slices.Contains(strings.Split(v, ","), hostname) {
			if v != "" {
				v += ","
			}
			out[i] = k + "=" + v + hostname
		}
	}
	return out
}
