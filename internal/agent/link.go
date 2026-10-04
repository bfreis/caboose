// Package agent is caboose-agent, the sandbox's end of the link to the host.
//
// `caboose-agent link` is run by the host's link helper through `docker exec
// -i`, and speaks agentproto on its stdin and stdout. While it runs it
// serves the in-container commands (`caboose-agent open`, `notify`, `ports`)
// on a Unix socket, reports the ports listening in the container, and
// connects the streams the host opens to them.
package agent

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/linkdebug"
)

// SocketPath is where the link serves the in-container commands. /tmp is the
// container's own, so the socket goes with the container.
const SocketPath = "/tmp/caboose-agent.sock"

// OpStatus is the in-container commands' own request: what the link knows
// of ports and forwards. It never reaches the host.
const OpStatus = "status"

// requestTimeout bounds a request to the host; opening a URL can wait on
// someone answering a dialog.
const requestTimeout = 2 * time.Minute

// Link is the running link.
type Link struct {
	sess     *agentproto.Session
	procRoot string
	interval time.Duration
	workDir  string

	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]chan agentproto.Message
	ports    []int
	forwards []agentproto.Forward

	changed chan []string

	// waiting are the streams the host is to open for SSH agent and
	// outbound proxy clients, by request; sshServing is set once the
	// socket is, egressServing once the proxy is asked for, and egressPort
	// is the proxy's, left out of the ports reported.
	waiting       map[uint64]chan *agentproto.Stream
	sshServing    bool
	egressServing bool
	egressPort    int
}

// Config is what a Link needs beyond its stdio.
type Config struct {
	Socket   string        // SocketPath, or another for tests
	ProcRoot string        // "/proc", or a fixture
	Interval time.Duration // between port scans
	WorkDir  string        // WorkDir, or another for tests
}

// RunLink runs the link over in and out until the host goes away.
func RunLink(in io.Reader, out io.WriteCloser, cfg Config) error {
	l := &Link{
		sess:     agentproto.NewSession(in, out, false),
		procRoot: cfg.ProcRoot,
		interval: cfg.Interval,
		workDir:  cmp.Or(cfg.WorkDir, WorkDir),
		pending:  map[uint64]chan agentproto.Message{},
		changed:  make(chan []string, changedQueue),

		waiting: map[uint64]chan *agentproto.Stream{},
	}
	l.sess.OnRefused(l.refused)
	defer l.sess.Close()
	// The socket accepts before the hello goes out, so a command run once
	// the host counts the link as up never finds it refusing. One that
	// cannot be made ends the link before the hello, with why on stderr.
	ln, err := listen(cfg.Socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := l.sess.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version}); err != nil {
		return err
	}
	if k := linkdebug.Get(); k.Raw != "" {
		// On a vm guest's console, or in what the host's docker exec
		// says when the link ends.
		fmt.Fprintf(os.Stderr, "caboose-agent: link: %s=%q: this side's streams take %d in flight\n", linkdebug.Var, k.Raw, l.sess.Window())
		l.sess.WatchStalls(func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "caboose-agent: "+format+"\n", args...)
		}, 5*time.Second)
	}
	go l.serveClients(ln)
	go l.watchPorts()
	go l.acceptStreams()
	go l.touchChanged()
	l.readControl()
	if err := l.sess.Err(); err != nil && !errors.Is(err, agentproto.ErrClosed) {
		return err
	}
	return nil
}

// listen takes over the socket path: a link that is still running is an old
// one, whose host has already gone to this one.
func listen(path string) (net.Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func (l *Link) readControl() {
	for b := range l.sess.Control() {
		m, err := agentproto.Decode(b)
		if err != nil {
			continue
		}
		switch m.Type {
		case agentproto.TypeHello:
			if m.Version != agentproto.Version {
				fmt.Fprintf(os.Stderr, "caboose-agent: the host speaks protocol %d, this agent %d\n", m.Version, agentproto.Version)
				return
			}
			if m.SSHAgent != "" {
				l.serveSSHAgent(m.SSHAgent)
			}
			if m.Egress != "" {
				l.serveEgress(m.Egress)
			}
		case agentproto.TypeResponse:
			l.mu.Lock()
			ch := l.pending[m.ID]
			delete(l.pending, m.ID)
			l.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case agentproto.TypeForwards:
			l.mu.Lock()
			l.forwards = m.Forwards
			l.mu.Unlock()
		case agentproto.TypeChanged:
			select {
			case l.changed <- m.Paths:
			default:
			}
		}
	}
}

// request sends m to the host and waits for its answer.
func (l *Link) request(m agentproto.Message) agentproto.Message {
	ch := make(chan agentproto.Message, 1)
	l.mu.Lock()
	l.nextID++
	m.ID = l.nextID
	l.pending[m.ID] = ch
	l.mu.Unlock()
	fail := func(err string) agentproto.Message {
		l.mu.Lock()
		delete(l.pending, m.ID)
		l.mu.Unlock()
		return agentproto.Message{Type: agentproto.TypeResponse, Error: err}
	}
	m.Type = agentproto.TypeRequest
	if err := l.sess.Send(m); err != nil {
		return fail("the host link is down: " + err.Error())
	}
	select {
	case r := <-ch:
		return r
	case <-l.sess.Done():
		return fail("the host link went down")
	case <-time.After(requestTimeout):
		return fail("the host did not answer")
	}
}

// serveClients answers the in-container commands: one request per
// connection, a line of JSON each way.
func (l *Link) serveClients(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go l.serveClient(c)
	}
}

func (l *Link) serveClient(c net.Conn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(c, agentproto.MaxPayload)).ReadBytes('\n')
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	var m agentproto.Message
	var resp agentproto.Message
	if err := json.Unmarshal(line, &m); err != nil {
		resp = agentproto.Message{Type: agentproto.TypeResponse, Error: "bad request"}
	} else if m.Op == OpStatus {
		l.mu.Lock()
		resp = agentproto.Message{Type: agentproto.TypeResponse, OK: true,
			Ports: slices.Clone(l.ports), Forwards: slices.Clone(l.forwards)}
		l.mu.Unlock()
	} else {
		resp = l.request(agentproto.Message{Op: m.Op, URL: m.URL, Title: m.Title, Text: m.Text})
	}
	b, _ := json.Marshal(resp)
	_, _ = c.Write(append(b, '\n'))
}

// watchPorts tells the host the listening ports whenever they change.
func (l *Link) watchPorts() {
	t := time.NewTicker(l.interval)
	defer t.Stop()
	var last []int
	sent := false
	for {
		ports := listeningPorts(l.procRoot)
		l.mu.Lock()
		// The outbound proxy is the agent's own, and never forwarded.
		ports = slices.DeleteFunc(ports, func(p int) bool { return p == l.egressPort })
		l.mu.Unlock()
		if !sent || !slices.Equal(ports, last) {
			l.mu.Lock()
			l.ports = ports
			l.mu.Unlock()
			if err := l.sess.Send(agentproto.Message{Type: agentproto.TypePorts, Ports: ports}); err != nil {
				return
			}
			last, sent = ports, true
		}
		select {
		case <-t.C:
		case <-l.sess.Done():
			return
		}
	}
}

// acceptStreams connects each stream the host opens to its port here, or
// to the SSH agent or outbound proxy client that asked for it.
func (l *Link) acceptStreams() {
	for {
		st, err := l.sess.Accept()
		if err != nil {
			return
		}
		go l.connect(st)
	}
}

func (l *Link) connect(st *agentproto.Stream) {
	var h agentproto.StreamHeader
	if err := json.Unmarshal(st.Header(), &h); err != nil {
		st.Close()
		return
	}
	if h.Request != 0 {
		// Handed over under the lock, so a client that gives up (forget,
		// in openStream) either finds it there and closes it, or has
		// taken it out of waiting first.
		l.mu.Lock()
		ch := l.waiting[h.Request]
		delete(l.waiting, h.Request)
		if ch != nil {
			ch <- st // buffered, and sent to once
		}
		l.mu.Unlock()
		if ch == nil {
			st.Close()
		}
		return
	}
	if !agentproto.ValidPort(h.Port) {
		st.Close()
		return
	}
	c, err := dialLocal(h.Port)
	if err != nil {
		st.Close()
		return
	}
	agentproto.Splice(st, c)
}

// refused hands nil to the client waiting for a stream the session reset
// unaccepted, so it fails at once rather than when its wait runs out.
func (l *Link) refused(header []byte) {
	var h agentproto.StreamHeader
	if json.Unmarshal(header, &h) != nil || h.Request == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch := l.waiting[h.Request]; ch != nil {
		delete(l.waiting, h.Request)
		ch <- nil // buffered, and sent to once
	}
}

// dialLocal connects to port on this container's loopback: IPv4 first,
// then IPv6. A server bound to 127.0.0.1 only is reachable this way, which
// it would not be through a published port.
func dialLocal(port int) (*net.TCPConn, error) {
	var err error
	for _, host := range []string{"127.0.0.1", "::1"} {
		var c net.Conn
		c, err = net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 5*time.Second)
		if err == nil {
			return c.(*net.TCPConn), nil
		}
	}
	return nil, err
}

// sshWait is how long a client of the SSH agent socket waits for the
// host's stream once the host said yes.
const sshWait = 10 * time.Second

// serveSSHAgent serves the host's SSH agent at path, as the host's hello
// asks: each client is a request to the host, which opens a stream to its
// own agent, or says why not. Once per link.
func (l *Link) serveSSHAgent(path string) {
	l.mu.Lock()
	serving := l.sshServing
	l.sshServing = true
	l.mu.Unlock()
	if serving {
		return
	}
	ln, err := listen(path)
	if err == nil {
		// Every user of the sandbox, as a mounted agent socket is.
		err = os.Chmod(path, 0o666)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "caboose-agent: cannot serve the SSH agent at %s: %v\n", path, err)
		return
	}
	go func() {
		<-l.sess.Done()
		ln.Close()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go l.sshClient(c)
		}
	}()
}

// sshClient asks the host for its agent, and splices c to the stream it
// opens.
func (l *Link) sshClient(c net.Conn) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return
	}
	st, _ := l.openStream(agentproto.Message{Op: agentproto.OpSSHAgent}, sshWait)
	if st == nil {
		c.Close()
		return
	}
	agentproto.Splice(st, uc)
}

// openStream sends the host request m, for which the host opens a stream
// if it agrees, and returns that stream; or nil and the host's refusal, or
// a response saying why there is none (Reason "", the link's own trouble).
// wait bounds the answer and then the stream, each.
func (l *Link) openStream(m agentproto.Message, wait time.Duration) (*agentproto.Stream, agentproto.Message) {
	ch := make(chan agentproto.Message, 1)
	stc := make(chan *agentproto.Stream, 1)
	l.mu.Lock()
	l.nextID++
	id := l.nextID
	l.pending[id] = ch
	l.waiting[id] = stc
	l.mu.Unlock()
	fail := func(why string) (*agentproto.Stream, agentproto.Message) {
		var late *agentproto.Stream
		l.mu.Lock()
		delete(l.pending, id)
		delete(l.waiting, id)
		select {
		case late = <-stc: // it came after all: the host is to hear it is not wanted
		default:
		}
		l.mu.Unlock()
		// Closed outside the lock: the reset is a write to the host,
		// which may wait, and readControl takes the lock for every
		// response.
		if late != nil {
			late.Close()
		}
		return nil, agentproto.Message{Type: agentproto.TypeResponse, ID: id, Error: why}
	}
	m.Type, m.ID = agentproto.TypeRequest, id
	if err := l.sess.Send(m); err != nil {
		return fail("the host link is down")
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	var r agentproto.Message
	select {
	case r = <-ch:
	case <-l.sess.Done():
		return fail("the host link went down")
	case <-t.C:
		return fail("the host did not answer")
	}
	if !r.OK {
		_, f := fail(cmp.Or(r.Error, "refused"))
		f.Reason = r.Reason
		return nil, f
	}
	t.Reset(wait)
	select {
	case st := <-stc:
		if st == nil {
			return fail("the host's connection was refused here: too many open at once")
		}
		return st, r
	case <-l.sess.Done():
		return fail("the host link went down")
	case <-t.C:
		return fail("the host said yes, but opened no connection")
	}
}
