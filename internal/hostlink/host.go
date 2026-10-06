// Package hostlink is the host's end of the link to caboose-agent: it
// forwards the sandbox's listening ports to this machine's localhost,
// opens URLs and shows notifications when the sandbox asks, under vm dials
// the sandbox's outbound connections from this machine (egress.go), and,
// where the user turned it on, runs its commands here (hostexec.go).
//
// Everything the agent sends is untrusted, as everything from the sandbox
// is. The host decides: which ports forward is config.toml's forward_ports,
// never the agent's; forwarding listens on 127.0.0.1 only; a URL opens only
// when it is http(s) and, by default, only after a dialog says yes; and
// every text the sandbox wrote is made printable and cut short before
// anything shows it; the outbound proxy reaches only egress_ports, and
// only public addresses unless egress_allow says otherwise. Commands run
// on this machine only with host_exec on (hostexec.go).
package hostlink

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/linkdebug"
	"github.com/bfreis/caboose/internal/proposal"
)

// The values of open_urls.
const (
	OpenAsk   = "ask"
	OpenAllow = "allow"
	OpenOff   = "off"
)

const (
	// maxForwards is the most ports forwarded at once, whatever the sandbox
	// listens on.
	maxForwards = 64
	maxURL      = 2048
	maxTitle    = 80
	maxText     = 500
	// requestEvery and requestBurst bound how often the sandbox may ask.
	requestEvery = 2 * time.Second
	requestBurst = 5
)

// Actions are what the host does for the sandbox: this machine's browser,
// notifications and dialogs. Tests fake them.
type Actions interface {
	OpenURL(u string) error
	Notify(title, text string) error
	// Confirm asks whoever is at this machine, and says whether they
	// agreed.
	Confirm(prompt string) (bool, error)
}

// Config is what the host side of a link needs.
type Config struct {
	Ports   PortSet
	OpenURL string // OpenAsk, OpenAllow or OpenOff
	Actions Actions
	Log     *log.Logger
	// Listen opens a forward's listener; nil is TCP on 127.0.0.1.
	Listen func(port int) (net.Listener, error)
	// Relay are the roots whose changes to relay into the container; nil
	// relays nothing.
	Relay []Root
	// Watch starts watching the roots' host directories; nil is
	// WatchRoots.
	Watch func(dirs []string) (Watcher, error)
	// SSHAgent is where, in the sandbox, the agent is to serve this
	// machine's SSH agent, SSHAuthSock (OpSSHAgent): under vm, where no
	// socket can be mounted. "" forwards none.
	SSHAgent, SSHAuthSock string
	// Egress is the outbound proxy the host offers the agent (OpConnect):
	// under vm, whose NAT reaches none of this machine's VPN routes. nil
	// offers none.
	Egress *Egress
	// HostExec runs commands on this machine for the sandbox
	// (OpHostExec): host_exec, in config.toml. nil offers none.
	HostExec *HostExec
	// Diagnose logs the windows the hellos settled on, and any stream or
	// write that stalls (agentproto's WatchStalls).
	Diagnose bool
}

// Host serves one session.
type Host struct {
	sess *agentproto.Session
	cfg  Config

	mu       sync.Mutex
	forwards map[int]*forward
	refused  map[int]string // port -> why it is not forwarded
	tokens   float64
	lastTick time.Time
	dialog   sync.Mutex // one dialog at a time
	ssh      int        // SSH agent connections open
	egress   egressState
	execs    hostExecState
}

// stallAfter is how long a stream waits for credit, or a write to the
// agent blocks, before Diagnose logs it.
const stallAfter = 5 * time.Second

// maxSSH is the most SSH agent connections open at once: git opens one or
// two per command.
const maxSSH = 16

type forward struct {
	port int
	ln   net.Listener
}

// ErrVersion is an agent that speaks another protocol.
var ErrVersion = errors.New("the sandbox's caboose-agent speaks another protocol version")

// ErrNoHello is a session that ended before the agent's hello: an agent
// that had nothing to serve yet, or none at all.
var ErrNoHello = errors.New("the link ended before the agent's hello")

// Run serves sess until it ends, and returns why. Forwards end with it.
func Run(sess *agentproto.Session, cfg Config) error {
	if cfg.Listen == nil {
		cfg.Listen = func(port int) (net.Listener, error) {
			return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		}
	}
	h := &Host{sess: sess, cfg: cfg, forwards: map[int]*forward{}, refused: map[int]string{},
		tokens: requestBurst, lastTick: time.Now()}
	defer h.closeAll()
	hello := agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version}
	if cfg.SSHAgent != "" && cfg.SSHAuthSock != "" {
		hello.SSHAgent = cfg.SSHAgent
	}
	hello.HostExec = cfg.HostExec != nil
	if cfg.Egress != nil {
		hello.Egress = cfg.Egress.listen()
		go h.countEgressEvery()
	}
	if err := sess.Send(hello); err != nil {
		return fmt.Errorf("%w (%v)", ErrNoHello, err)
	}
	helloed := false
	// The relay's watcher, started at the hello, lives as long as the session.
	var watcher Watcher
	defer func() {
		if watcher != nil {
			watcher.Close()
		}
	}()
	for b := range sess.Control() {
		m, err := agentproto.Decode(b)
		if err != nil {
			cfg.Log.Printf("dropping a bad message: %v", err)
			continue
		}
		if !helloed {
			if m.Type != agentproto.TypeHello {
				return errors.New("the agent did not start with a hello")
			}
			if m.Version != agentproto.Version {
				return fmt.Errorf("%w: %d, this caboose %d ('caboose restart' rebuilds the image)", ErrVersion, m.Version, agentproto.Version)
			}
			helloed = true
			if cfg.Diagnose && cfg.Log != nil {
				note := ""
				if k := linkdebug.Get(); k.Raw != "" {
					note = fmt.Sprintf(" (%s=%q)", linkdebug.Var, k.Raw)
				}
				cfg.Log.Printf("windows: this side's streams take %d in flight, the agent's %d%s", sess.Window(), sess.PeerWindow(), note)
				sess.WatchStalls(cfg.Log.Printf, stallAfter)
			}
			if watcher = h.watch(); watcher != nil {
				go h.relayChanges(watcher)
			}
			continue
		}
		switch m.Type {
		case agentproto.TypePorts:
			h.reconcile(m.Ports)
		case agentproto.TypeRequest:
			go h.answer(m)
		}
	}
	if !helloed {
		return fmt.Errorf("%w (%v)", ErrNoHello, sess.Err())
	}
	return sess.Err()
}

// watch starts watching the relay's roots, or says why it cannot: the link
// still serves everything else.
func (h *Host) watch() Watcher {
	if len(h.cfg.Relay) == 0 {
		return nil
	}
	start := h.cfg.Watch
	if start == nil {
		start = WatchRoots
	}
	var dirs []string
	for _, r := range h.cfg.Relay {
		dirs = append(dirs, r.Host)
	}
	w, err := start(dirs)
	if err != nil {
		h.cfg.Log.Printf("cannot watch %s (%v): changes made here will not reach watchers in the sandbox", strings.Join(dirs, ", "), err)
		return nil
	}
	h.cfg.Log.Printf("relaying changes under %s into the sandbox", strings.Join(dirs, ", "))
	return w
}

func (h *Host) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for p, f := range h.forwards {
		f.ln.Close()
		delete(h.forwards, p)
	}
}

// reconcile makes the forwards match what listens in the sandbox, then
// tells the agent what came of it.
func (h *Host) reconcile(ports []int) {
	listening := map[int]bool{}
	for _, p := range ports {
		if agentproto.ValidPort(p) {
			listening[p] = true
		}
	}
	h.mu.Lock()
	for p, f := range h.forwards {
		if !listening[p] {
			f.ln.Close()
			delete(h.forwards, p)
			h.cfg.Log.Printf("port %d: no longer listening in the sandbox, forward stopped", p)
		}
	}
	h.refused = map[int]string{}
	for _, p := range slices.Sorted(maps.Keys(listening)) {
		if _, ok := h.forwards[p]; ok {
			continue
		}
		switch {
		case !h.cfg.Ports.Has(p):
			h.refused[p] = "not in forward_ports"
		case len(h.forwards) >= maxForwards:
			h.refused[p] = fmt.Sprintf("already forwarding %d ports", maxForwards)
		default:
			ln, err := h.cfg.Listen(p)
			if err != nil {
				h.refused[p] = "the port is in use on the host"
				continue
			}
			f := &forward{port: p, ln: ln}
			h.forwards[p] = f
			go h.serve(f)
			h.cfg.Log.Printf("port %d: forwarded to localhost:%d", p, p)
		}
	}
	msg := agentproto.Message{Type: agentproto.TypeForwards}
	for p := range listening {
		if _, ok := h.forwards[p]; ok {
			msg.Forwards = append(msg.Forwards, agentproto.Forward{Port: p, HostPort: p})
		} else {
			msg.Forwards = append(msg.Forwards, agentproto.Forward{Port: p, Reason: h.refused[p]})
		}
	}
	h.mu.Unlock()
	slices.SortFunc(msg.Forwards, func(a, b agentproto.Forward) int { return a.Port - b.Port })
	_ = h.sess.Send(msg)
}

// serve connects each connection to the forward's port to a new stream.
func (h *Host) serve(f *forward) {
	hdr, _ := json.Marshal(agentproto.StreamHeader{Port: f.port})
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		st, err := h.sess.Open(hdr)
		if err != nil {
			c.Close()
			continue
		}
		tc, ok := c.(*net.TCPConn)
		if !ok {
			c.Close()
			st.Close()
			continue
		}
		go agentproto.Splice(st, tc)
	}
}

// allow takes a token from the request bucket, or says the sandbox is
// asking too often.
func (h *Host) allow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	h.tokens = min(requestBurst, h.tokens+now.Sub(h.lastTick).Seconds()/requestEvery.Seconds())
	h.lastTick = now
	if h.tokens < 1 {
		return false
	}
	h.tokens--
	return true
}

func (h *Host) answer(m agentproto.Message) {
	var err error
	if m.Op == agentproto.OpSSHAgent {
		// Not the bucket's: a git command opens one or two, and it only
		// ever reaches this machine's agent, which asks its own questions.
		err = h.sshAgent(m.ID)
	} else if m.Op == agentproto.OpConnect {
		// Its own bucket and limits (egress.go): a page load opens
		// several at once.
		err = h.connect(m.ID, m.Host, m.Port)
	} else if m.Op == agentproto.OpHostExec {
		// Bounded by how many run at once, not the bucket: a session
		// runs one after another.
		err = h.hostExec(m.ID)
	} else if !h.allow() {
		err = errors.New("too many requests; try again shortly")
	} else {
		switch m.Op {
		case agentproto.OpOpen:
			err = h.open(m.URL)
		case agentproto.OpNotify:
			err = h.notify(m.Title, m.Text)
		default:
			err = fmt.Errorf("unknown request %q", clip(m.Op, 40))
		}
	}
	r := agentproto.Message{Type: agentproto.TypeResponse, ID: m.ID, OK: err == nil}
	if err != nil {
		r.Error, r.Reason = err.Error(), egressReason(err)
	}
	_ = h.sess.Send(r)
}

// sshAgent opens a stream for request id, connected to this machine's
// SSH agent.
func (h *Host) sshAgent(id uint64) error {
	if h.cfg.SSHAgent == "" || h.cfg.SSHAuthSock == "" {
		return errors.New("the host forwards no SSH agent")
	}
	h.mu.Lock()
	if h.ssh >= maxSSH {
		h.mu.Unlock()
		return fmt.Errorf("already %d SSH agent connections open", maxSSH)
	}
	h.ssh++
	h.mu.Unlock()
	done := func() {
		h.mu.Lock()
		h.ssh--
		h.mu.Unlock()
	}
	c, err := net.DialTimeout("unix", h.cfg.SSHAuthSock, 5*time.Second)
	if err != nil {
		done()
		h.cfg.Log.Printf("ssh agent: cannot reach %s: %v", h.cfg.SSHAuthSock, err)
		return errors.New("the host's SSH agent does not answer")
	}
	hdr, _ := json.Marshal(agentproto.StreamHeader{Request: id})
	st, err := h.sess.Open(hdr)
	if err != nil {
		c.Close()
		done()
		return err
	}
	go func() {
		defer done()
		agentproto.Splice(st, c.(*net.UnixConn))
	}()
	return nil
}

func (h *Host) open(raw string) error {
	if h.cfg.OpenURL == OpenOff {
		return errors.New("opening URLs is off on the host (open_urls)")
	}
	u, err := CheckURL(raw)
	if err != nil {
		return err
	}
	if h.cfg.OpenURL != OpenAllow {
		h.dialog.Lock()
		ok, err := h.cfg.Actions.Confirm("The caboose sandbox asks to open this URL in your browser:\n\n" + u)
		h.dialog.Unlock()
		if err != nil {
			return fmt.Errorf("could not ask on the host: %v", err)
		}
		if !ok {
			h.cfg.Log.Printf("open %s: declined", u)
			return errors.New("declined on the host")
		}
	}
	h.cfg.Log.Printf("open %s", u)
	return h.cfg.Actions.OpenURL(u)
}

// CheckURL is raw if it is a URL the host may open: http or https, with a
// host, not too long, nothing unprintable in it. What it returns is the
// URL re-encoded, which is what gets shown and opened.
func CheckURL(raw string) (string, error) {
	if len(raw) > maxURL {
		return "", fmt.Errorf("the URL is over %d bytes", maxURL)
	}
	if proposal.Printable(raw) != raw {
		return "", errors.New("the URL holds unprintable characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("not a URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("only http and https URLs open on the host, not %q", clip(u.Scheme, 20))
	}
	if u.Host == "" || u.User != nil {
		return "", errors.New("the URL must name a host, and no user or password")
	}
	return u.String(), nil
}

func (h *Host) notify(title, text string) error {
	return Notify(h.cfg.Actions, title, text)
}

// Notify shows text under title with actions, as the sandbox's notify
// request is shown: made printable and cut short first, since either may
// hold what the sandbox wrote.
func Notify(actions Actions, title, text string) error {
	title, text = clip(proposal.Printable(title), maxTitle), clip(proposal.Printable(text), maxText)
	if text == "" {
		return errors.New("nothing to say")
	}
	if title == "" {
		title = "caboose"
	}
	return actions.Notify(title, text)
}

// clip cuts s to at most n bytes, on a character boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
