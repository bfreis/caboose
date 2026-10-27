// Package hostlink is the host's end of the link to caboose-agent: it
// forwards the sandbox's listening ports to this machine's localhost,
// opens URLs and shows notifications when the sandbox asks, under vm dials
// the sandbox's outbound connections from this machine (egress.go), and,
// where the user turned it on, runs its commands here (hostexec.go).
//
// Everything the agent sends is untrusted, as everything from the sandbox
// is. The host decides: which ports forward is config.toml's forward_ports,
// never the agent's; forwarding listens on 127.0.0.1 only; every other
// crossing -- a URL opened, an outbound connection, a host command -- is
// decided by the link's Decider, the environment's broker, by the policy
// config.toml and a person's grants make (internal/policy). One that asks
// a person is refused for now as pending, never waited on, with the
// number of the request filed for it (requests.go); a URL opens only when
// it is http(s), and the outbound proxy dials only the addresses checked.
// Every text the sandbox wrote is made printable and cut short before
// anything shows it, and every request it may make is bounded and
// rate-limited. Commands run on this machine only with host_exec on
// (hostexec.go).
package hostlink

import (
	"bytes"
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

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/linkdebug"
	"github.com/bfreis/caboose/internal/policy"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/uiport"
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
	// HostListens says whether a program on this machine accepts
	// connections on 127.0.0.1:port, which a forward there would take
	// over; nil dials the port (portcheck.go). It is asked once for each
	// time the sandbox starts listening on a port, for every port that
	// would be forwarded, forward_ports' included.
	HostListens func(port int) bool
	// PortRecheck is how often the ports are decided again while one
	// waits for a grant, a check or the end of a timed grant; 0 is every
	// 5 seconds.
	PortRecheck time.Duration
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
	// Decide makes the decisions on what the sandbox asks; nil decides as
	// these settings say.
	Decide Decider
	// Trust is the environment's configured trust (config.toml's trust),
	// which the fallback decision (Decide nil) judges by. Anything but
	// "fallible", unset included, is decided as hostile: a link that
	// cannot tell the trust fails closed.
	Trust string
	// Execs, when set, also tracks this link's host commands, so they can
	// be counted and killed apart from every other link's.
	Execs *Execs
	// Gate, when set, admits this link's host-exec requests: once it is
	// drained, they are refused. nil admits every one.
	Gate *ExecGate
	// AliasTmp is where aliases with no cwd of their own run, each in a
	// fresh directory made in it and removed after: CABOOSE_HOME's
	// daemon directory's alias-tmp. "" offers no aliases (OpAliasRun,
	// OpAliases), and so does a Config with no Decide.
	AliasTmp string
	// Checkpoints takes the checkpoints the sandbox asks for
	// (OpCheckpoint), and CheckpointLimits bounds how often, one per
	// environment (a Host's own when nil). nil offers none.
	Checkpoints      Checkpointer
	CheckpointLimits *CheckpointLimits
	// CheckpointRequests is [link] checkpoint_requests, which the
	// fallback decision (Decide nil) decides OpCheckpoint by.
	CheckpointRequests string
	// Mods, when set, is told the agent's count of Claude Code sessions
	// and of those with caboose's mod (TypeMods), checked, whenever it
	// changes.
	Mods func(agentproto.ModsInfo)
	// Forwarded, when set, is told the sandbox ports now forwarded to the
	// host (the same on the host), sorted, each time the set may have
	// changed. It must not block.
	Forwarded func(ports []int)
}

// Host serves one session.
type Host struct {
	sess *agentproto.Session
	cfg  Config

	mu       sync.Mutex
	forwards map[int]*forward
	refused  map[int]string // port -> why it is not forwarded
	uiLogged bool           // a web UI port's refusal was logged
	ports    portsState     // what is known of the ports the sandbox listens on
	tokens   float64
	lastTick time.Time
	ssh      int // SSH agent connections open
	egress   egressState
	execs    hostExecState
	execWG   sync.WaitGroup // host commands' goroutines, which Run waits for
	closing  bool           // Run is waiting for them: hostExec adds no more
	reqs     requestsState  // OpBrokerRequests' bucket and waits
	alias    aliasState     // OpAliasRun's bucket
	cpBusy   bool           // an OpCheckpoint is being served
	lastMods *agentproto.ModsInfo
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
	// until is when the grant that opened it ends; zero for a port in
	// forward_ports and for a once grant, which last as long as the
	// sandbox listens.
	until time.Time
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
	go h.recheckPorts()
	// The session is over by the time Run returns, and with it every
	// command it ran (runHostExec): Run returns once they are gone, so a
	// link that stops leaves none behind.
	defer h.waitExecs()
	hello := agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version}
	if cfg.SSHAgent != "" && cfg.SSHAuthSock != "" {
		hello.SSHAgent = cfg.SSHAgent
	}
	hello.HostExec = cfg.HostExec != nil
	hello.Broker = cfg.Decide != nil
	hello.AliasesOn = h.aliasesOn()
	hello.CheckpointsOn = h.checkpointsOn()
	for _, r := range cfg.Relay {
		hello.Roots = append(hello.Roots, r.Container)
	}
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
		case agentproto.TypeMods:
			h.mods(m.Mods)
		}
	}
	if !helloed {
		return fmt.Errorf("%w (%v)", ErrNoHello, sess.Err())
	}
	return sess.Err()
}

// waitExecs stops host exec taking more commands, then waits for those it
// took: a WaitGroup may not be added to once it is waited on.
func (h *Host) waitExecs() {
	h.mu.Lock()
	h.closing = true
	h.mu.Unlock()
	h.execWG.Wait()
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
	h.ports.evalMu.Lock()
	defer h.ports.evalMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ports.closed = true
	for p, f := range h.forwards {
		f.ln.Close()
		delete(h.forwards, p)
	}
}

// reconcile makes the forwards match what listens in the sandbox, then
// tells the agent what came of it. It runs inline in the control loop, so
// what it waits for is bounded (evaluate).
func (h *Host) reconcile(ports []int) {
	listening := map[int]bool{}
	for _, p := range ports {
		if agentproto.ValidPort(p) {
			listening[p] = true
		}
	}
	g := &h.ports
	g.evalMu.Lock()
	h.mu.Lock()
	g.listening = listening
	for p, f := range h.forwards {
		if !listening[p] {
			f.ln.Close()
			delete(h.forwards, p)
			h.cfg.Log.Printf("port %d: no longer listening in the sandbox, forward stopped", p)
		}
	}
	// A port that stops listening forgets what was learnt of it: the
	// next listen is checked again.
	for p := range g.state {
		if !listening[p] {
			delete(g.state, p)
		}
	}
	h.mu.Unlock()
	h.evaluateLocked()
	g.evalMu.Unlock()
	h.sendForwards(false)
}

// sendForwards tells the agent which of the ports it listens on are
// forwarded and why not the rest, and Forwarded the ports forwarded. With
// changedOnly, only if that differs from what the agent was last sent.
// The message is cut to fit the protocol's bound: the forwarded ports
// first, then the others, lowest first; a port left out is one the agent
// is told nothing of.
func (h *Host) sendForwards(changedOnly bool) {
	g := &h.ports
	g.sendMu.Lock()
	defer g.sendMu.Unlock()
	// The snapshot is taken between evaluates, never halfway through one.
	g.evalMu.Lock()
	h.mu.Lock()
	forwarded := slices.Sorted(maps.Keys(h.forwards))
	var others []int
	for p := range g.listening {
		if _, ok := h.forwards[p]; !ok {
			others = append(others, p)
		}
	}
	slices.Sort(others)
	msg := agentproto.Message{Type: agentproto.TypeForwards}
	// What the rest of the message takes, with room to spare.
	budget := agentproto.MaxPayload - 1024
	add := func(f agentproto.Forward) bool {
		b, _ := json.Marshal(f)
		if len(b)+1 > budget {
			return false
		}
		budget -= len(b) + 1
		msg.Forwards = append(msg.Forwards, f)
		return true
	}
	full := false
	for _, p := range forwarded {
		if !add(agentproto.Forward{Port: p, HostPort: p}) {
			full = true
			break
		}
	}
	for _, p := range others {
		if full || !add(agentproto.Forward{Port: p, Reason: h.refused[p]}) {
			break
		}
	}
	h.mu.Unlock()
	g.evalMu.Unlock()
	slices.SortFunc(msg.Forwards, func(a, b agentproto.Forward) int { return a.Port - b.Port })
	sig, _ := json.Marshal(msg.Forwards)
	if changedOnly && g.sent != nil && bytes.Equal(g.sent, sig) {
		return
	}
	if h.cfg.Forwarded != nil {
		h.cfg.Forwarded(forwarded)
	}
	// Only what reached the agent counts as sent: a message that did not
	// is sent again by the next reconcile or recheck.
	if h.sess.Send(msg) == nil {
		g.sent = sig
	}
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
	r := agentproto.Message{Type: agentproto.TypeResponse, ID: m.ID}
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
	} else if m.Op == agentproto.OpAliasRun {
		// Its own bucket, and host exec's bound on how many run at once.
		err = h.aliasRun(m.ID, m.Exec)
	} else if m.Op == agentproto.OpAliases {
		// The listing bucket.
		err = h.aliases(&r)
	} else if m.Op == agentproto.OpBrokerRequests {
		// Its own bucket, and a bound on the waits (requests.go): a
		// wait must hold up nothing else.
		err = h.brokerRequests(m, &r)
	} else if !h.allow() {
		err = errors.New("too many requests; try again shortly")
	} else {
		switch m.Op {
		case agentproto.OpOpen:
			err = h.open(m.URL)
		case agentproto.OpNotify:
			err = h.notify(m.Title, m.Text)
		case agentproto.OpBrokerRequest:
			err = h.brokerRequest(m.Ask, &r)
		case agentproto.OpCheckpoint:
			err = h.checkpoint(m.Paths, &r)
		default:
			err = fmt.Errorf("unknown request %q", clip(m.Op, 40))
		}
	}
	r.OK = err == nil
	if err != nil {
		r.Error, r.Reason, r.RequestID = err.Error(), egressReason(err), requestOf(err)
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

// open serves OpOpen: the URL checked, its opening decided, and opened
// at once when allowed. One that asks is never waited on here: it is
// refused for now as pending, and the broker opens it once a person
// allows it (from the host's dialog or its CLI).
func (h *Host) open(raw string) error {
	u, err := CheckURL(raw)
	if err != nil {
		// With open_urls off, that is what is said, whatever the URL.
		if h.cfg.Decide == nil && h.cfg.OpenURL == OpenOff {
			return errors.New("opening URLs is off on the host (open_urls)")
		}
		return err
	}
	o, _ := h.decider().Decide(policy.Request{Kind: policy.KindOpenURL, URL: u, Origin: Origin(u)})
	switch o.Verdict {
	case broker.Allowed:
		h.cfg.Log.Printf("open %s", u)
		return h.cfg.Actions.OpenURL(u)
	case broker.Pending:
		h.cfg.Log.Printf("open %s: pending #%d", u, o.Request)
		return &refusal{reason: agentproto.ReasonPending, request: o.Request,
			msg: fmt.Sprintf("pending #%d: %s; the host opens the URL once a person allows it there", o.Request, o.Reason)}
	}
	h.cfg.Log.Printf("open %s: refused: %s", u, clip(o.Reason, 300))
	return errors.New(o.Reason)
}

// Origin is u's origin, scheme://host[:port] in lower case, as
// open_urls_allow names one; u is CheckURL's.
func Origin(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return strings.ToLower(p.Scheme + "://" + p.Host)
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
	// By the port alone, whatever the host: a browser takes loopback in
	// many spellings (127.1, 2130706433, [::ffff:127.0.0.1], localhost.,
	// x.localhost), and a name may resolve to it.
	if p := effectivePort(u); p < 0 || uiport.Is(p) {
		return "", errors.New("the host's caboose web UI is never opened from a sandbox: a URL to port 7171 or 7172 is refused")
	}
	return u.String(), nil
}

// effectivePort is the port a browser connects to for u, http or https:
// the one it names, else the scheme's; -1 when it names one that is not a
// number a browser would take.
func effectivePort(u *url.URL) int {
	ps := u.Port()
	if ps == "" {
		if u.Scheme == "https" {
			return 443
		}
		return 80
	}
	p, err := strconv.Atoi(ps)
	if err != nil || p < 0 || p > 65535 {
		return -1
	}
	return p
}

func (h *Host) notify(title, text string) error {
	return SandboxNotify(h.cfg.Actions, title, text)
}

// SandboxNotifier is what Actions also offer when they tell a sandbox's
// notification from a host component's: SandboxNotify is given the
// sandbox's title (empty when it gave none) and text, made printable and
// cut short.
type SandboxNotifier interface {
	SandboxNotify(title, text string) error
}

// Notify shows text under title with actions, as the sandbox's notify
// request is shown: made printable and cut short first, since either may
// hold what the sandbox wrote.
func Notify(actions Actions, title, text string) error {
	title, text, err := notice(title, text)
	if err != nil {
		return err
	}
	if title == "" {
		title = "caboose"
	}
	return actions.Notify(title, text)
}

// SandboxNotify shows what a sandbox asked to: as Notify does, but
// through actions' SandboxNotify when they offer one, so that they know
// the sandbox wrote it.
func SandboxNotify(actions Actions, title, text string) error {
	s, ok := actions.(SandboxNotifier)
	if !ok {
		return Notify(actions, title, text)
	}
	title, text, err := notice(title, text)
	if err != nil {
		return err
	}
	return s.SandboxNotify(title, text)
}

// notice is a notification's title and text made printable and cut short.
func notice(title, text string) (string, string, error) {
	title, text = clip(proposal.Printable(title), maxTitle), clip(proposal.Printable(text), maxText)
	if text == "" {
		return "", "", errors.New("nothing to say")
	}
	return title, text, nil
}

// clip cuts s to at most n bytes, on a character boundary.
func clip(s string, n int) string { return proposal.Clip(s, n) }
