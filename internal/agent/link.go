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

	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]chan agentproto.Message
	ports    []int
	forwards []agentproto.Forward
}

// Config is what a Link needs beyond its stdio.
type Config struct {
	Socket   string        // SocketPath, or another for tests
	ProcRoot string        // "/proc", or a fixture
	Interval time.Duration // between port scans
}

// RunLink runs the link over in and out until the host goes away.
func RunLink(in io.Reader, out io.WriteCloser, cfg Config) error {
	l := &Link{
		sess:     agentproto.NewSession(in, out, false),
		procRoot: cfg.ProcRoot,
		interval: cfg.Interval,
		pending:  map[uint64]chan agentproto.Message{},
	}
	defer l.sess.Close()
	if err := l.sess.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version}); err != nil {
		return err
	}
	ln, err := listen(cfg.Socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	go l.serveClients(ln)
	go l.watchPorts()
	go l.acceptStreams()
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

// acceptStreams connects each stream the host opens to its port here.
func (l *Link) acceptStreams() {
	for {
		st, err := l.sess.Accept()
		if err != nil {
			return
		}
		go connect(st)
	}
}

func connect(st *agentproto.Stream) {
	var h agentproto.StreamHeader
	if err := json.Unmarshal(st.Header(), &h); err != nil || !agentproto.ValidPort(h.Port) {
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
