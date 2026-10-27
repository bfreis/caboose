package hostlink

// Which of the ports the sandbox listens on are forwarded. A port in
// forward_ports, or one a grant covers, is forwarded only if no program on
// this machine already accepts connections on 127.0.0.1:port: the forward's
// listener would otherwise be bound ahead of it, or beside it on another
// address, and take the connections meant for that program. The check is a
// dial, made once for each time the sandbox starts listening on a port,
// again once a forward of it ends with its grant, and once for each
// request for one, through a bucket, since the sandbox decides how often
// it listens. A port outside forward_ports is decided by
// the Decider in two steps, so that only a port that could be allowed is
// ever dialled: first without the check, which says "ask" (nothing is
// dialled, the sandbox is told how to request it), or "checked first";
// then with the check's result. The link only decides here: a listen never
// files a request, only an explicit one does (requests.go).

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/policy"
	"github.com/bfreis/caboose/internal/uiport"
)

const (
	// probeTimeout is how long a dial of 127.0.0.1 waits: a port whose
	// backlog is full does not answer, and counts as in use.
	probeTimeout = 250 * time.Millisecond
	// probeBurst and probeEvery bound the dials: a burst, then one every
	// probeEvery.
	probeBurst = 32
	probeEvery = 500 * time.Millisecond
	// maxPortsDecided is how many of the ports the sandbox listens on and
	// this link does not forward are decided at each evaluate, the lowest
	// first: the sandbox decides how many it lists, and each may cost the
	// Decider a decision. The rest are refused with reasonTooMany.
	maxPortsDecided = 4 * maxForwards
	// maxReason bounds the reason sent for one port.
	maxReason = 160

	reasonHostListens = "a program on the host listens on this port: forwarding it would take that program's localhost connections; listen on another port"
	reasonProbeRate   = "the host checks ports too often: it tries this one again shortly"
	reasonNotForward  = "not in forward_ports ([link] of config.toml on the host)"
	reasonInUse       = "the port is in use on the host"
)

// reasonTooMany is the refusal of a port past maxPortsDecided.
var reasonTooMany = fmt.Sprintf("the host decides at most %d of the ports it does not forward at once, the lowest first", maxPortsDecided)

// askReason is what a port the policy would ask about is told: short, since
// the sandbox may list many, and saying how to ask.
func askReason(port int) string {
	return fmt.Sprintf("not in forward_ports: `caboose-agent request port --port %d` asks the host to forward it", port)
}

// defaultPortRecheck is how often the link decides again while a port
// waits for a grant, a check, or the end of a timed one, unless
// Config.PortRecheck says otherwise.
const defaultPortRecheck = 5 * time.Second

// portsState is what a Host knows of the ports the sandbox listens on, all
// under Host.mu but the mutexes and sent. Every change to listening, to
// the Host's forwards and to closed is made with evalMu held too
// (reconcile, evaluate, closeAll), so none changes while an evaluate runs.
type portsState struct {
	evalMu sync.Mutex // one evaluate at a time
	// sendMu makes one forwards message at a time, from its snapshot to
	// its send, so that the last one sent is the last one made.
	sendMu    sync.Mutex
	listening map[int]bool
	// state is what the check found, for as long as the port listens and
	// no forward of it has ended.
	state  map[int]*portCheck
	closed bool
	// retry is set when some port waits for something that changes
	// without the sandbox listening on anything new.
	retry bool
	// sent is the last forwards message the agent was sent, its ports as
	// JSON; under sendMu.
	sent []byte
	// tokens and last are the check's bucket.
	tokens float64
	last   time.Time
}

type portCheck struct{ hostListens bool }

// probeHost reports whether a program on this machine accepts connections
// on 127.0.0.1:port. Only a refusal means no: any other failure, a timeout
// included, counts as in use, so the check fails closed.
func probeHost(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), probeTimeout)
	if err == nil {
		c.Close()
		return true
	}
	return !errors.Is(err, syscall.ECONNREFUSED)
}

func (h *Host) hostListens(port int) bool {
	if h.cfg.HostListens != nil {
		return h.cfg.HostListens(port)
	}
	return probeHost(port)
}

// takeProbe takes a token from the check's bucket.
func (h *Host) takeProbe() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := &h.ports
	now := time.Now()
	if g.last.IsZero() {
		g.tokens = probeBurst
	} else {
		g.tokens = min(probeBurst, g.tokens+now.Sub(g.last).Seconds()/probeEvery.Seconds())
	}
	g.last = now
	if g.tokens < 1 {
		return false
	}
	g.tokens--
	return true
}

// forwarding reports whether this link forwards port now.
func (h *Host) forwarding(port int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.forwards[port]
	return ok
}

// portTodo is a port that may be forwarded, once checked.
type portTodo struct {
	port   int
	config bool // in forward_ports: no grant needed
	until  time.Time
}

// evaluate decides again which ports to forward. It holds h.mu only to
// read and to commit, never while dialling or deciding, and its dials run
// together and take at most probeTimeout.
func (h *Host) evaluate() {
	h.ports.evalMu.Lock()
	defer h.ports.evalMu.Unlock()
	h.evaluateLocked()
}

// evaluateLocked is evaluate with evalMu held, so the ports listening, the
// forwards and closed stay as they are while it runs (portsState). A port
// outside forward_ports has its forward's listener bound before the
// decision that may use a once grant up, and nothing can come between that
// decision and the forward: a grant is never spent on a forward that does
// not open.
func (h *Host) evaluateLocked() {
	g := &h.ports
	now := time.Now()

	h.mu.Lock()
	if g.closed {
		h.mu.Unlock()
		return
	}
	for p, f := range h.forwards {
		if !f.until.IsZero() && !now.Before(f.until) {
			f.ln.Close()
			delete(h.forwards, p)
			// The check made before this forward is not reused: another
			// forward of the port is checked again.
			delete(g.state, p)
			h.cfg.Log.Printf("port %d: its grant ended, forward stopped", p)
		}
	}
	var unforwarded []int
	for p := range g.listening {
		if _, ok := h.forwards[p]; !ok {
			unforwarded = append(unforwarded, p)
		}
	}
	slices.Sort(unforwarded)
	room := maxForwards - len(h.forwards)
	h.mu.Unlock()

	refused := map[int]string{}
	if len(unforwarded) > maxPortsDecided {
		for _, p := range unforwarded[maxPortsDecided:] {
			refused[p] = reasonTooMany
		}
		unforwarded = unforwarded[:maxPortsDecided]
	}
	retry := false
	var todo []portTodo
	for _, p := range unforwarded {
		switch {
		// The web UI's ports are never forwarded, whatever Ports says:
		// config.toml refuses them, and this holds for a set read
		// another way. Logged once a link, as the agent decides how
		// often it lists them.
		case uiport.Is(p):
			refused[p] = "the host's caboose web UI port, never forwarded"
			h.mu.Lock()
			first := !h.uiLogged
			h.uiLogged = true
			h.mu.Unlock()
			if first {
				h.cfg.Log.Printf("port %d: never forwarded: it is %s", p, uiport.What)
			}
		case h.cfg.Ports.Has(p):
			todo = append(todo, portTodo{port: p, config: true})
		case p < 1024 || h.cfg.Decide == nil:
			refused[p] = reasonNotForward
			if p < 1024 {
				refused[p] += ", and a port below 1024 forwards only when forward_ports names it"
			}
		default:
			// Asking never files a request and never uses a grant up.
			o, err := h.cfg.Decide.Decide(policy.Request{Kind: policy.KindPort, Port: p})
			switch {
			case err != nil:
				refused[p] = "cannot decide: " + clip(printableHost(err.Error()), maxReason)
			case o.Rule == policy.RuleAskPort:
				refused[p] = askReason(p)
				retry = true
			case o.Rule == policy.RulePortUnchecked || o.Verdict == broker.Allowed:
				todo = append(todo, portTodo{port: p})
			default:
				refused[p] = clip(o.Reason, maxReason)
			}
		}
	}

	// The cap counts what is granted too.
	if len(todo) > room {
		for _, t := range todo[max(room, 0):] {
			refused[t.port] = fmt.Sprintf("already forwarding %d ports", maxForwards)
		}
		todo = todo[:max(room, 0)]
	}

	// Check what has not been, as many at once as the bucket allows. Every
	// check is made before any listener of this evaluate is bound, so none
	// finds one of the link's own.
	h.mu.Lock()
	need := make([]int, 0, len(todo))
	for _, t := range todo {
		if g.state[t.port] == nil {
			need = append(need, t.port)
		}
	}
	h.mu.Unlock()
	var fresh []int
	for _, p := range need {
		if h.takeProbe() {
			fresh = append(fresh, p)
		}
	}
	found := make([]bool, len(fresh))
	var wg sync.WaitGroup
	for i, p := range fresh {
		wg.Add(1)
		go func() {
			defer wg.Done()
			found[i] = h.hostListens(p)
		}()
	}
	wg.Wait()
	h.mu.Lock()
	if g.state == nil {
		g.state = map[int]*portCheck{}
	}
	for i, p := range fresh {
		g.state[p] = &portCheck{hostListens: found[i]}
	}
	checks := map[int]portCheck{}
	for _, t := range todo {
		if c := g.state[t.port]; c != nil {
			checks[t.port] = *c
		}
	}
	h.mu.Unlock()

	for _, t := range todo {
		c, ok := checks[t.port]
		switch {
		case !ok:
			refused[t.port] = reasonProbeRate
			retry = true
		case c.hostListens:
			refused[t.port] = reasonHostListens
		default:
			if why, ask := h.openForward(t); why != "" {
				refused[t.port] = why
				retry = retry || ask
			}
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.refused = refused
	g.retry = retry
	for _, f := range h.forwards {
		if !f.until.IsZero() {
			g.retry = true
		}
	}
}

// openForward binds t's listener and, for a port outside forward_ports,
// then decides it with the check's result, which may use a once grant up:
// only once the listener is bound, so that a grant is spent only on a
// forward that opens. It says why not when it opens none, and whether the
// policy asks. evalMu is held.
func (h *Host) openForward(t portTodo) (string, bool) {
	ln, err := h.cfg.Listen(t.port)
	if err != nil {
		return reasonInUse, false
	}
	if !t.config {
		o, err := h.cfg.Decide.Decide(policy.Request{Kind: policy.KindPort, Port: t.port, HostChecked: true})
		switch {
		case err != nil:
			ln.Close()
			return "cannot decide: " + clip(printableHost(err.Error()), maxReason), false
		case o.Verdict != broker.Allowed && o.Rule == policy.RuleAskPort:
			ln.Close()
			return askReason(t.port), true
		case o.Verdict != broker.Allowed:
			ln.Close()
			return clip(o.Reason, maxReason), false
		}
		t.until = o.Until
	}
	f := &forward{port: t.port, ln: ln, until: t.until}
	h.mu.Lock()
	h.forwards[t.port] = f
	h.mu.Unlock()
	go h.serve(f)
	h.cfg.Log.Printf("port %d: forwarded to localhost:%d", t.port, t.port)
	return "", false
}

// recheckPorts decides again every Config.PortRecheck while some port
// waits for something that changes on its own: a grant, or a timed one's
// end. The agent only lists ports when they change, so nothing else would.
// A timed forward so ends within one interval of its grant's end.
func (h *Host) recheckPorts() {
	every := h.cfg.PortRecheck
	if every <= 0 {
		every = defaultPortRecheck
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-h.sess.Done():
			return
		case <-t.C:
		}
		h.mu.Lock()
		retry := h.ports.retry
		h.mu.Unlock()
		if retry {
			h.evaluate()
			h.sendForwards(true)
		}
	}
}

// askPort is the request for a port forward, with the check's result when
// it can be allowed at all: a web UI port, or one below 1024 that
// forward_ports does not name, is refused by the policy before anything
// would be dialled.
func (h *Host) askPort(port int) (policy.Request, error) {
	if !agentproto.ValidPort(port) {
		return policy.Request{}, fmt.Errorf("%d is not a port", port)
	}
	r := policy.Request{Kind: policy.KindPort, Port: port}
	if uiport.Is(port) || port < 1024 && !h.cfg.Ports.Has(port) {
		return r, nil
	}
	if !h.takeProbe() {
		return policy.Request{}, errors.New("the host checks ports too often; try again shortly")
	}
	r.HostChecked = true
	r.HostListens = h.hostListens(port)
	return r, nil
}
