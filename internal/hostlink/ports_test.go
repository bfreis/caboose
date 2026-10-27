package hostlink

import (
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/policy"
)

// portHost is a Host built directly, with the seams a test of the ports
// needs: what listens on the host, and the listeners opened. Nothing is
// bound for a port but through Listen, and the check never dials.
type portHost struct {
	*Host
	mu        sync.Mutex
	probed    []int
	listened  []int
	listens   map[int]bool // what the host's own programs listen on
	lns       []net.Listener
	listenErr error
	msgs      chan agentproto.Message
	last      *agentproto.Message
}

func newPortHost(t *testing.T, ports string, d Decider) *portHost {
	t.Helper()
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	hostSess := agentproto.NewSession(hr, hw, true)
	agentSess := agentproto.NewSession(ar, aw, false)
	t.Cleanup(func() { hostSess.Close(); agentSess.Close() })
	set, err := ParsePorts(ports)
	if err != nil {
		t.Fatal(err)
	}
	p := &portHost{listens: map[int]bool{}, msgs: make(chan agentproto.Message, 256)}
	cfg := Config{
		Ports: set,
		Log:   log.New(io.Discard, "", 0),
		Listen: func(port int) (net.Listener, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.listened = append(p.listened, port)
			if p.listenErr != nil {
				return nil, p.listenErr
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err == nil {
				p.lns = append(p.lns, ln)
			}
			return ln, err
		},
		HostListens: func(port int) bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.probed = append(p.probed, port)
			return p.listens[port]
		},
	}
	if d != nil {
		cfg.Decide = d
	}
	p.Host = &Host{sess: hostSess, forwards: map[int]*forward{}, refused: map[int]string{}, cfg: cfg}
	t.Cleanup(func() {
		p.Host.closeAll()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, ln := range p.lns {
			ln.Close()
		}
	})
	go func() {
		for b := range agentSess.Control() {
			if m, err := agentproto.Decode(b); err == nil {
				select {
				case p.msgs <- m:
				default:
				}
			}
		}
	}()
	return p
}

func (p *portHost) probes() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.probed...)
}

func (p *portHost) listenCalls() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.listened...)
}

// reconcile lists ports to the Host and waits for the forwards message it
// answers with.
func (p *portHost) reconcile(t *testing.T, ports []int) {
	t.Helper()
	for len(p.msgs) > 0 {
		<-p.msgs
	}
	done := make(chan struct{})
	go func() { p.Host.reconcile(ports); close(done) }()
	for {
		select {
		case m := <-p.msgs:
			if m.Type == agentproto.TypeForwards {
				p.last = &m
				<-done
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no forwards message")
		}
	}
}

// reason is why port is not forwarded, as the agent was told by the last
// reconcile; "" if it is forwarded.
func (p *portHost) reason(t *testing.T, port int) string {
	t.Helper()
	for _, f := range p.last.Forwards {
		if f.Port == port {
			return f.Reason
		}
	}
	t.Fatalf("port %d not in %+v", port, p.last.Forwards)
	return ""
}

func (p *portHost) forwarded(port int) bool {
	p.Host.mu.Lock()
	defer p.Host.mu.Unlock()
	_, ok := p.Host.forwards[port]
	return ok
}

func count(l []int, port int) int {
	n := 0
	for _, x := range l {
		if x == port {
			n++
		}
	}
	return n
}

// A port is checked once for each time the sandbox starts listening on it,
// however often the agent lists it, and again after it stops and starts.
func TestPortCheckedOncePerListen(t *testing.T) {
	p := newPortHost(t, "3000", nil)
	p.listenErr = io.ErrUnexpectedEOF // in use: the port stays unforwarded and is decided every time
	for range 3 {
		p.reconcile(t, []int{3000})
		if got := p.reason(t, 3000); got != reasonInUse {
			t.Fatalf("reason %q", got)
		}
	}
	if n := count(p.probes(), 3000); n != 1 {
		t.Errorf("checked %d times in 3 reconciles, want 1", n)
	}
	p.reconcile(t, nil)
	p.reconcile(t, []int{3000})
	if n := count(p.probes(), 3000); n != 2 {
		t.Errorf("checked %d times after the port went and came back, want 2", n)
	}
}

// A program on the host listening there refuses the forward, a port in
// forward_ports included, and nothing is bound.
func TestPortHostListenerRefuses(t *testing.T) {
	p := newPortHost(t, "3000", nil)
	p.listens[3000] = true
	p.reconcile(t, []int{3000})
	if got := p.reason(t, 3000); !strings.Contains(got, "a program on the host listens on this port") {
		t.Errorf("reason %q", got)
	}
	if n := p.probes(); len(n) != 1 {
		t.Errorf("checks %v", n)
	}
	if l := p.listenCalls(); len(l) != 0 {
		t.Errorf("listened for %v", l)
	}
}

// A port in forward_ports forwards once the check finds no program of the
// host's, with no Decider asked.
func TestPortInForwardPortsForwardsAfterCheck(t *testing.T) {
	fd := newFakeDecider()
	p := newPortHost(t, "3000", fd)
	p.reconcile(t, []int{3000})
	if got := p.reason(t, 3000); got != "" {
		t.Errorf("reason %q", got)
	}
	if !p.forwarded(3000) || len(p.probes()) != 1 {
		t.Errorf("forwarded %v, checks %v", p.forwarded(3000), p.probes())
	}
	if len(fd.decided) != 0 || len(fd.filed) != 0 {
		t.Errorf("decided %+v, filed %+v", fd.decided, fd.filed)
	}
}

// The web UI's ports are never checked, and a port below 1024 that
// forward_ports does not name is refused without a check or a Decider
// asked, even by one that allows everything.
func TestPortNoCheckForUIOrPrivileged(t *testing.T) {
	fd := newFakeDecider()
	fd.decideFn = func(policy.Request) broker.Outcome { return broker.Outcome{Verdict: broker.Allowed} }
	p := newPortHost(t, "3000", fd)
	p.reconcile(t, []int{7171, 80})
	if got := p.reason(t, 80); !strings.HasPrefix(got, "not in forward_ports") || !strings.Contains(got, "below 1024") {
		t.Errorf("port 80: %q", got)
	}
	if got := p.reason(t, 7171); !strings.Contains(got, "web UI") {
		t.Errorf("port 7171: %q", got)
	}
	if n := p.probes(); len(n) != 0 {
		t.Errorf("checked %v", n)
	}
	if len(fd.decided) != 0 || len(p.listenCalls()) != 0 {
		t.Errorf("decided %+v, listened %v", fd.decided, p.listenCalls())
	}
}

// A granted port is decided in two steps, the second with the check's
// result, and then forwarded; the link never files a request.
func TestPortGrantedTwoPhases(t *testing.T) {
	fd := newFakeDecider()
	fd.decideFn = func(r policy.Request) broker.Outcome {
		if !r.HostChecked {
			return broker.Outcome{Verdict: broker.Denied, Rule: policy.RulePortUnchecked, Reason: "unchecked"}
		}
		return broker.Outcome{Verdict: broker.Allowed, Rule: "grant:hours"}
	}
	p := newPortHost(t, "3000", fd)
	p.reconcile(t, []int{4321})
	if got := p.reason(t, 4321); got != "" {
		t.Errorf("reason %q", got)
	}
	if !p.forwarded(4321) {
		t.Error("not forwarded")
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.decided) != 2 || fd.decided[0].HostChecked || !fd.decided[1].HostChecked || fd.decided[1].HostListens {
		t.Errorf("decided %+v", fd.decided)
	}
	for _, r := range fd.decided {
		if r.Kind != policy.KindPort || r.Port != 4321 {
			t.Errorf("decided %+v", r)
		}
	}
	if len(fd.filed) != 0 {
		t.Errorf("filed %+v", fd.filed)
	}
}

// A port the policy asks about is refused with the policy's words, which
// tell the sandbox how to ask, and is not checked.
func TestPortAskRefused(t *testing.T) {
	fd := newFakeDecider()
	fd.decideFn = func(policy.Request) broker.Outcome {
		return broker.Outcome{Verdict: broker.Denied, Rule: policy.RuleAskPort,
			Reason: "port 4321 is not in forward_ports: `caboose-agent request port --port 4321` asks the host to forward it"}
	}
	p := newPortHost(t, "3000", fd)
	p.reconcile(t, []int{4321})
	if got := p.reason(t, 4321); !strings.Contains(got, "caboose-agent request port") {
		t.Errorf("reason %q", got)
	}
	if len(p.probes()) != 0 || len(p.listenCalls()) != 0 || len(fd.filed) != 0 {
		t.Errorf("checks %v, listened %v, filed %+v", p.probes(), p.listenCalls(), fd.filed)
	}
}

// A port that asked is decided again on its own, without the sandbox
// listing anything, and forwards once a grant exists.
func TestPortRecheckOpensAfterGrant(t *testing.T) {
	var mu sync.Mutex
	granted := false
	fd := newFakeDecider()
	fd.decideFn = func(r policy.Request) broker.Outcome {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case !granted:
			return broker.Outcome{Verdict: broker.Denied, Rule: policy.RuleAskPort, Reason: "ask"}
		case !r.HostChecked:
			return broker.Outcome{Verdict: broker.Denied, Rule: policy.RulePortUnchecked, Reason: "unchecked"}
		}
		return broker.Outcome{Verdict: broker.Allowed}
	}
	p := newPortHost(t, "3000", fd)
	p.Host.cfg.PortRecheck = 10 * time.Millisecond
	go p.recheckPorts()
	p.reconcile(t, []int{4321})
	if p.forwarded(4321) {
		t.Fatal("forwarded before the grant")
	}
	mu.Lock()
	granted = true
	mu.Unlock()
	waitFor(t, func() bool { return p.forwarded(4321) })
	if n := count(p.probes(), 4321); n != 1 {
		t.Errorf("checked %d times", n)
	}
}

// A forward a timed grant opened is closed when the grant ends, and the
// port is decided again.
func TestPortUntilClosesForward(t *testing.T) {
	end := time.Now().Add(150 * time.Millisecond)
	fd := newFakeDecider()
	fd.decideFn = func(r policy.Request) broker.Outcome {
		if time.Now().After(end) {
			return broker.Outcome{Verdict: broker.Denied, Rule: policy.RuleAskPort, Reason: "ask"}
		}
		if !r.HostChecked {
			return broker.Outcome{Verdict: broker.Denied, Rule: policy.RulePortUnchecked, Reason: "unchecked"}
		}
		return broker.Outcome{Verdict: broker.Allowed, Rule: "grant:hours", Until: end}
	}
	p := newPortHost(t, "3000", fd)
	p.Host.cfg.PortRecheck = 10 * time.Millisecond
	go p.recheckPorts()
	p.reconcile(t, []int{4321})
	if !p.forwarded(4321) {
		t.Fatal("not forwarded")
	}
	// Between evaluates, never halfway through one: the forward is closed
	// at an evaluate's start, and its reason set at its end.
	waitFor(t, func() bool {
		fwd, why := p.settled(4321)
		return !fwd && why == askReason(4321)
	})
}

// settled is whether port is forwarded and why not, as an evaluate left
// them.
func (p *portHost) settled(port int) (bool, string) {
	p.Host.ports.evalMu.Lock()
	defer p.Host.ports.evalMu.Unlock()
	p.Host.mu.Lock()
	defer p.Host.mu.Unlock()
	_, ok := p.Host.forwards[port]
	return ok, p.Host.refused[port]
}

// A forward that ends with its grant drops the check made before it: the
// next forward of the port, under a later grant, is checked again.
func TestPortCheckedAgainAfterGrantEnds(t *testing.T) {
	end := time.Now().Add(100 * time.Millisecond)
	fd := newFakeDecider()
	fd.decideFn = func(r policy.Request) broker.Outcome {
		if !r.HostChecked {
			return broker.Outcome{Verdict: broker.Denied, Rule: policy.RulePortUnchecked, Reason: "unchecked"}
		}
		if time.Now().Before(end) {
			return broker.Outcome{Verdict: broker.Allowed, Rule: "grant:hours", Until: end}
		}
		return broker.Outcome{Verdict: broker.Allowed, Rule: "grant:hours", Until: end.Add(time.Hour)}
	}
	p := newPortHost(t, "3000", fd)
	p.Host.cfg.PortRecheck = 10 * time.Millisecond
	go p.recheckPorts()
	p.reconcile(t, []int{4321})
	if !p.forwarded(4321) {
		t.Fatal("not forwarded")
	}
	waitFor(t, func() bool { return count(p.probes(), 4321) == 2 })
	waitFor(t, func() bool { fwd, _ := p.settled(4321); return fwd })
}

// consumingDecider allows a port once it is checked, as a grant does, and
// counts those decisions: the ones that would use a once grant up.
type consumingDecider struct {
	*fakeDecider
	mu       sync.Mutex
	consumed int
	// during, when set, runs inside each such decision.
	during func()
}

func newConsumingDecider() *consumingDecider {
	c := &consumingDecider{fakeDecider: newFakeDecider()}
	c.decideFn = func(r policy.Request) broker.Outcome {
		if !r.HostChecked {
			return broker.Outcome{Verdict: broker.Denied, Rule: policy.RulePortUnchecked, Reason: "unchecked"}
		}
		return broker.Outcome{Verdict: broker.Allowed, Rule: "grant:once"}
	}
	return c
}

func (c *consumingDecider) Decide(r policy.Request) (broker.Outcome, error) {
	if r.HostChecked {
		c.mu.Lock()
		c.consumed++
		during := c.during
		c.mu.Unlock()
		if during != nil {
			during()
		}
	}
	return c.fakeDecider.Decide(r)
}

func (c *consumingDecider) uses() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consumed
}

// A once grant is used up only on a forward that opens: never when the
// forward's listener cannot be bound, and never on a port that stops
// listening between the decision and the forward.
func TestPortOnceGrantSpentOnlyOnForward(t *testing.T) {
	t.Run("listen fails", func(t *testing.T) {
		cd := newConsumingDecider()
		p := newPortHost(t, "3000", cd)
		p.listenErr = io.ErrUnexpectedEOF
		p.reconcile(t, []int{4321})
		if got := p.reason(t, 4321); got != reasonInUse {
			t.Errorf("reason %q", got)
		}
		if n := cd.uses(); n != 0 {
			t.Errorf("%d grant-using decisions, no forward opened", n)
		}
	})
	t.Run("stops listening", func(t *testing.T) {
		cd := newConsumingDecider()
		p := newPortHost(t, "3000", cd)
		var opened sync.WaitGroup
		opened.Add(1)
		var once sync.Once
		cd.during = func() {
			once.Do(func() {
				// The sandbox stops listening while the decision is made.
				go func() { p.Host.reconcile(nil); opened.Done() }()
				time.Sleep(50 * time.Millisecond)
			})
		}
		var logged strings.Builder
		var logMu sync.Mutex
		p.Host.cfg.Log = log.New(writerFunc(func(b []byte) (int, error) {
			logMu.Lock()
			defer logMu.Unlock()
			return logged.Write(b)
		}), "", 0)
		p.reconcile(t, []int{4321})
		opened.Wait()
		logMu.Lock()
		defer logMu.Unlock()
		if n, f := cd.uses(), strings.Count(logged.String(), "forwarded to localhost:4321"); n != f {
			t.Errorf("%d grant-using decisions, %d forwards opened:\n%s", n, f, logged.String())
		}
	})
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(b []byte) (int, error) { return w(b) }

// However many ports the sandbox lists, an evaluate decides at most
// maxPortsDecided of them, and the forwards message stays within the
// protocol's bound and reaches the agent.
func TestPortManyListedBounded(t *testing.T) {
	fd := newFakeDecider()
	fd.decideFn = func(r policy.Request) broker.Outcome {
		return broker.Outcome{Verdict: broker.Denied, Rule: policy.RuleAskPort,
			Reason: strings.Repeat("a long reason from the policy ", 20)}
	}
	p := newPortHost(t, "none", fd)
	var all []int
	for i := 0; i < 10000; i++ {
		all = append(all, 20000+i)
	}
	p.reconcile(t, all)
	fd.mu.Lock()
	decided := len(fd.decided)
	fd.mu.Unlock()
	if decided > maxPortsDecided {
		t.Errorf("%d decisions for one list", decided)
	}
	if n := len(p.last.Forwards); n < maxPortsDecided {
		t.Errorf("%d ports in the forwards message", n)
	}
	if got := p.reason(t, 20000); got != askReason(20000) {
		t.Errorf("port 20000: %q", got)
	}
	if got := p.reason(t, 20000+maxPortsDecided); got != reasonTooMany {
		t.Errorf("port past the bound: %q", got)
	}
}

// The check itself: a port something on 127.0.0.1 accepts on is in use,
// one nothing listens on is free.
func TestProbeHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if !probeHost(ln.Addr().(*net.TCPAddr).Port) {
		t.Error("a listening port reported free")
	}
	// A port just closed: another program could take it in between, which
	// is unlikely enough here.
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()
	if probeHost(port) {
		t.Error("a closed port reported in use")
	}
}

// The checks come from a bucket: a sandbox listing many ports gets
// probeBurst of them checked at once, and the rest told to wait.
func TestPortCheckBucketRunsOut(t *testing.T) {
	p := newPortHost(t, "3000-3059", nil)
	var all []int
	for i := 0; i < 60; i++ {
		all = append(all, 3000+i)
	}
	p.reconcile(t, all)
	if n := len(p.probes()); n < probeBurst || n > probeBurst+2 {
		t.Errorf("%d checks, want about %d", n, probeBurst)
	}
	p.Host.mu.Lock()
	defer p.Host.mu.Unlock()
	waiting := 0
	for _, why := range p.Host.refused {
		if why == reasonProbeRate {
			waiting++
		}
	}
	if waiting < 60-probeBurst-2 {
		t.Errorf("%d ports told to wait", waiting)
	}
}

// A request for a port is filed with the host's own check of it, never
// the sandbox's word; a web UI port or a port below 1024 that
// forward_ports does not name is filed unchecked, for the policy to
// refuse, and costs no check.
func TestPortRequestFiledWithCheck(t *testing.T) {
	fd := newFakeDecider()
	fd.set(policy.KindPort, broker.Outcome{Verdict: broker.Pending, Request: 9, Reason: "ask"})
	var mu sync.Mutex
	var probed []int
	a := startExecHost(t, Config{Ports: PortSet{}, Actions: &fakeActions{}, Decide: fd,
		HostListens: func(p int) bool { mu.Lock(); probed = append(probed, p); mu.Unlock(); return p == 4322 }})
	for _, c := range []struct {
		port           int
		checked, binds bool
	}{{4321, true, false}, {4322, true, true}, {80, false, false}, {7171, false, false}} {
		r := a.request(agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: &agentproto.Ask{Kind: "port", Port: c.port}})
		if r.Verdict != agentproto.VerdictPending || r.RequestID != 9 {
			t.Errorf("port %d: %+v", c.port, r)
		}
		fd.mu.Lock()
		got := fd.filed[len(fd.filed)-1].Request
		fd.mu.Unlock()
		if got.Kind != policy.KindPort || got.Port != c.port || got.HostChecked != c.checked || got.HostListens != c.binds {
			t.Errorf("port %d filed %+v", c.port, got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(probed) != 2 {
		t.Errorf("checked %v, want 4321 and 4322 only", probed)
	}
}

// A request for a port this link already forwards is answered allowed
// without filing anything.
func TestPortRequestAlreadyForwarded(t *testing.T) {
	fd := newFakeDecider()
	set, _ := ParsePorts("4321")
	forwarded := make(chan []int, 8)
	a := startExecHost(t, Config{Ports: set, Actions: &fakeActions{}, Decide: fd,
		HostListens: func(int) bool { return false },
		Listen:      func(int) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		Forwarded:   func(p []int) { forwarded <- p }})
	if err := a.sess.Send(agentproto.Message{Type: agentproto.TypePorts, Ports: []int{4321}}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-forwarded:
		if len(p) != 1 || p[0] != 4321 {
			t.Fatalf("forwarded %v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not forwarded")
	}
	r := a.request(agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: &agentproto.Ask{Kind: "port", Port: 4321}})
	if r.Verdict != agentproto.VerdictAllowed || !r.OK {
		t.Errorf("got %+v", r)
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.filed) != 0 {
		t.Errorf("filed %+v", fd.filed)
	}
}
