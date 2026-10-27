package hostlink

import (
	"context"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agent"
	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/policy"
)

// fakeDecider is a Decider the test sets: an Outcome per kind (denied when
// it names none), the requests and asks it was given, and a list of
// requests whose decisions the test makes.
type fakeDecider struct {
	mu      sync.Mutex
	verdict map[policy.Kind]broker.Outcome
	decided []policy.Request
	filed   []broker.Ask
	reqs    []broker.RequestInfo
	seq     uint64
	moved   chan struct{} // closed and replaced when seq moves
	waiting int           // Waits running now
	waited  int           // Waits that ever ran
	// decideFn, when set, answers Decide instead of verdict.
	decideFn func(policy.Request) broker.Outcome
}

func newFakeDecider() *fakeDecider {
	return &fakeDecider{verdict: map[policy.Kind]broker.Outcome{}, seq: 1, moved: make(chan struct{})}
}

func (f *fakeDecider) set(k policy.Kind, o broker.Outcome) {
	f.mu.Lock()
	f.verdict[k] = o
	f.mu.Unlock()
}

func (f *fakeDecider) outcome(k policy.Kind) broker.Outcome {
	if o, ok := f.verdict[k]; ok {
		return o
	}
	return broker.Outcome{Verdict: broker.Denied, Reason: "the fake says no"}
}

func (f *fakeDecider) Decide(r policy.Request) (broker.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decided = append(f.decided, r)
	if f.decideFn != nil {
		return f.decideFn(r), nil
	}
	return f.outcome(r.Kind), nil
}

func (f *fakeDecider) File(a broker.Ask) (broker.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filed = append(f.filed, a)
	return f.outcome(a.Request.Kind), nil
}

// List lists as the broker does: pending ones, those decided after
// Since, or all, newest first.
func (f *fakeDecider) List(q broker.Query) ([]broker.RequestInfo, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []broker.RequestInfo
	for i := len(f.reqs) - 1; i >= 0; i-- {
		r := f.reqs[i]
		if q.Session != "" && r.Session != q.Session {
			continue
		}
		if r.State == broker.StatePending || q.All && q.Since == 0 || q.Since > 0 && r.Seq > q.Since {
			out = append(out, r)
		}
	}
	return out, f.seq, nil
}

func (f *fakeDecider) Wait(ctx context.Context, since uint64) (uint64, error) {
	f.mu.Lock()
	f.waiting++
	f.waited++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.waiting--
		f.mu.Unlock()
	}()
	for {
		f.mu.Lock()
		seq, ch := f.seq, f.moved
		f.mu.Unlock()
		if seq > since {
			return seq, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return seq, nil
		}
	}
}

func (f *fakeDecider) Aliases() ([]broker.AliasInfo, error)     { return nil, nil }
func (f *fakeDecider) Alias(string) (policy.Alias, bool, error) { return policy.Alias{}, false, nil }

func (f *fakeDecider) count() (int, int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.decided), len(f.filed), f.waiting, f.waited
}

// add adds a request, pending, for session.
func (f *fakeDecider) add(id uint64, session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, broker.RequestInfo{ID: id, Kind: policy.KindEgress, State: broker.StatePending, Session: session, Effect: "connect to x:443"})
}

// decide decides request id, as a person's answer would.
func (f *fakeDecider) decide(id uint64, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	for i := range f.reqs {
		if f.reqs[i].ID == id {
			f.reqs[i].State, f.reqs[i].Seq, f.reqs[i].DecidedAt = state, f.seq, time.Now()
		}
	}
	close(f.moved)
	f.moved = make(chan struct{})
}

// A Decider has the last word over the settings: a URL open_urls allows,
// and a command host_exec runs, are refused when it says so.
func TestDeciderDecides(t *testing.T) {
	fa := &fakeActions{answer: true}
	fd := newFakeDecider()
	socket := linked(t, Config{Ports: PortSet{}, OpenURL: OpenAllow, Actions: fa, Decide: fd})
	if err := agent.Open(socket, "https://example.com/"); err == nil || !strings.Contains(err.Error(), "the fake says no") || len(fa.opened) != 0 || fa.confirm != 0 {
		t.Fatalf("open: %v, opened %v, asked %d", err, fa.opened, fa.confirm)
	}

	dir, cfg := execRoot(t)
	cfg.Decide = fd
	a := startExecHost(t, cfg)
	if _, err := a.open(); err == nil || !strings.Contains(err.Error(), "the fake says no") {
		t.Fatalf("exec: %v", err)
	}
	fd.mu.Lock()
	last := fd.decided[len(fd.decided)-1]
	fd.mu.Unlock()
	if last.Kind != policy.KindHostExec {
		t.Fatalf("host exec decided as %+v", last)
	}
	// Allowed, it runs.
	fd.set(policy.KindHostExec, broker.Outcome{Verdict: broker.Allowed})
	if r := a.run(agentproto.ExecRequest{Argv: []string{"sh", "-c", "pwd -P"}, Dir: "/work"}, ""); r.code != 0 || r.stdout != dir+"\n" {
		t.Fatalf("allowed exec: %+v", r)
	}
}

// Host exec is decided before anything is run or a stream opened: a
// policy that refuses it, as trust = hostile does, refuses it even with
// host_exec on, and the command never starts.
func TestHostExecRefusedByPolicy(t *testing.T) {
	_, cfg := execRoot(t)
	fd := newFakeDecider()
	fd.set(policy.KindHostExec, broker.Outcome{Verdict: broker.Denied, Reason: "trust = hostile refuses host_exec"})
	cfg.Decide = fd
	cfg.Execs = NewExecs()
	a := startExecHost(t, cfg)
	if _, err := a.open(); err == nil || !strings.Contains(err.Error(), "trust = hostile") {
		t.Fatalf("got %v", err)
	}
	// A pending answer is no allow either.
	fd.set(policy.KindHostExec, broker.Outcome{Verdict: broker.Pending, Request: 3, Reason: "asks"})
	if _, err := a.open(); err == nil {
		t.Fatal("a pending host exec ran")
	}
	if cfg.Execs.Running() != 0 {
		t.Fatal("something runs")
	}
}

// Without one, the settings decide, as they always have, through the
// policy; what would ask is refused, since no one would answer.
func TestSettingsDecider(t *testing.T) {
	ports, _ := ParsePortsOf("egress_ports", "443")
	allow, _ := ParseAllow("git.corp")
	deny, _ := ParseAllow("blocked.example")
	cfg := &Config{Trust: "fallible", OpenURL: OpenAsk, Egress: &Egress{Ports: ports, Allow: allow, Deny: deny}}
	d := newSettingsDecider(cfg)
	url := func(mode string) broker.Outcome {
		dd := newSettingsDecider(&Config{Trust: "fallible", OpenURL: mode})
		o, _ := dd.Decide(policy.Request{Kind: policy.KindOpenURL, URL: "https://example.com/x", Origin: "https://example.com"})
		return o
	}
	if o := url(OpenAllow); o.Verdict != broker.Allowed {
		t.Errorf("open_urls allow: %+v", o)
	}
	if o := url(OpenAsk); o.Verdict != broker.Denied || !strings.Contains(o.Reason, noBroker) {
		t.Errorf("open_urls ask with no broker: %+v", o)
	}
	if o := url(OpenOff); o.Verdict != broker.Denied {
		t.Errorf("open_urls off: %+v", o)
	}
	eg := func(host string, addrs ...string) broker.Outcome {
		r := policy.Request{Kind: policy.KindEgress, Host: host, Port: 443}
		for _, a := range addrs {
			r.Addrs = append(r.Addrs, netip.MustParseAddr(a))
		}
		o, _ := d.Decide(r)
		return o
	}
	if o := eg("github.com", "140.82.112.3"); o.Verdict != broker.Allowed {
		t.Errorf("public: %+v", o)
	}
	if o := eg("git.corp", "10.1.2.3"); o.Verdict != broker.Allowed {
		t.Errorf("allowed private: %+v", o)
	}
	for _, c := range [][]string{{"x.example", "10.0.0.7"}, {"x.example", "127.0.0.1"}, {"blocked.example", "140.82.112.3"}} {
		if o := eg(c[0], c[1]); o.Verdict != broker.Denied {
			t.Errorf("%v: %+v", c, o)
		}
	}
	if o, _ := d.Decide(policy.Request{Kind: policy.KindHostExec}); o.Verdict != broker.Denied || o.Reason != agentproto.HostExecOff {
		t.Errorf("host exec off: %+v", o)
	}
	if o, _ := newSettingsDecider(&Config{Trust: "fallible", HostExec: &HostExec{}}).Decide(policy.Request{Kind: policy.KindHostExec}); o.Verdict != broker.Allowed {
		t.Errorf("host exec on: %+v", o)
	}
	if o, _ := d.File(broker.Ask{Request: policy.Request{Kind: policy.KindOpenURL}}); o.Verdict != broker.Denied {
		t.Errorf("filed with no broker: %+v", o)
	}
	if _, _, err := d.List(broker.Query{}); err == nil {
		t.Error("listed with no broker")
	}
}

// Each link's commands are its own: killing one set leaves another's.
func TestExecsArePerLink(t *testing.T) {
	_, cfg := execRoot(t)
	one, two := NewExecs(), NewExecs()
	c1, c2 := cfg, cfg
	c1.Execs, c2.Execs = one, two
	p1, _, _ := startSleepers(t, startExecHost(t, c1))
	p2, _, _ := startSleepers(t, startExecHost(t, c2))
	if one.Running() != 1 || two.Running() != 1 || HostExecsRunning() < 2 {
		t.Fatalf("running %d, %d, %d in all", one.Running(), two.Running(), HostExecsRunning())
	}
	one.Kill()
	gone(t, p1)
	for _, pid := range p2 {
		if !processAlive(pid) {
			t.Fatalf("killing one link's commands killed another's (%d)", pid)
		}
	}
	two.Kill()
	gone(t, p2)
}

// processAlive reports whether pid runs, and is no zombie.
func processAlive(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	st := strings.TrimSpace(string(out))
	return err == nil && st != "" && !strings.HasPrefix(st, "Z")
}

// Once Run waits for its commands, host exec takes no more: adding to a
// WaitGroup being waited on is not allowed.
func TestHostExecRefusedOnceClosing(t *testing.T) {
	h := &Host{cfg: Config{Trust: "fallible", HostExec: &HostExec{}}}
	h.waitExecs()
	if err := h.hostExec(1); err == nil || !strings.Contains(err.Error(), "closing") {
		t.Fatalf("host exec while closing: %v", err)
	}
}

// A link that cannot tell the environment's trust (none given, or one it
// does not know) decides as hostile: host exec refused, a URL asks (so,
// refused with no broker), egress asks for anything no entry names.
func TestSettingsDeciderFailsClosed(t *testing.T) {
	ports, _ := ParsePortsOf("egress_ports", "443")
	allow, _ := ParseAllow("git.corp")
	for _, trust := range []string{"", "bogus", "FALLIBLE", "hostile"} {
		cfg := &Config{Trust: trust, HostExec: &HostExec{}, OpenURL: OpenAllow,
			Egress: &Egress{Ports: ports, Allow: allow}}
		d := newSettingsDecider(cfg)
		if o, _ := d.Decide(policy.Request{Kind: policy.KindHostExec}); o.Verdict != broker.Denied {
			t.Errorf("trust %q: host exec: %+v", trust, o)
		}
		if o, _ := d.Decide(policy.Request{Kind: policy.KindOpenURL, URL: "https://example.com/x", Origin: "https://example.com"}); o.Verdict != broker.Denied || !strings.Contains(o.Reason, noBroker) {
			t.Errorf("trust %q: open_urls allow still asks: %+v", trust, o)
		}
		pub := policy.Request{Kind: policy.KindEgress, Host: "github.com", Port: 443, Addrs: []netip.Addr{netip.MustParseAddr("140.82.112.3")}}
		if o, _ := d.Decide(pub); o.Verdict != broker.Denied {
			t.Errorf("trust %q: public egress no entry names: %+v", trust, o)
		}
		named := policy.Request{Kind: policy.KindEgress, Host: "git.corp", Port: 443, Addrs: []netip.Addr{netip.MustParseAddr("140.82.112.4")}}
		if o, _ := d.Decide(named); o.Verdict != broker.Allowed {
			t.Errorf("trust %q: egress an entry names: %+v", trust, o)
		}
	}
}

// Through a whole link with no Decider and no trust given, host exec is
// refused too.
func TestLinkWithoutTrustRefusesHostExec(t *testing.T) {
	h := &Host{cfg: Config{HostExec: &HostExec{}}}
	if o, _ := h.decider().Decide(policy.Request{Kind: policy.KindHostExec}); o.Verdict != broker.Denied {
		t.Fatalf("host exec with no trust: %+v", o)
	}
}
