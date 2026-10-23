package hostlink

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
)

// send sends a request m, numbered, and returns its number.
func (a *execAgent) send(m agentproto.Message) uint64 {
	a.t.Helper()
	a.mu.Lock()
	a.nextID++
	id := a.nextID
	a.mu.Unlock()
	a.chans(id)
	m.Type, m.ID = agentproto.TypeRequest, id
	if err := a.sess.Send(m); err != nil {
		a.t.Fatal(err)
	}
	return id
}

// answer is the host's response to request id, within d.
func (a *execAgent) answer(id uint64, d time.Duration) (agentproto.Message, bool) {
	select {
	case r := <-a.chans(id).answer:
		return r, true
	case <-time.After(d):
		return agentproto.Message{}, false
	}
}

// request sends m and returns the host's response.
func (a *execAgent) request(m agentproto.Message) agentproto.Message {
	a.t.Helper()
	r, ok := a.answer(a.send(m), 5*time.Second)
	if !ok {
		a.t.Fatalf("no answer to %s", m.Op)
	}
	return r
}

// blockingActions never answers a dialog.
type blockingActions struct {
	fakeActions
	block chan struct{}
}

func (b *blockingActions) Confirm(string) (bool, error) {
	<-b.block
	return false, nil
}

// A URL the policy asks about is refused at once as pending, with the
// request's number: the agent is never kept waiting, and the host never
// asks itself (the broker's dialog does), nor opens it.
func TestOpenPending(t *testing.T) {
	fd := newFakeDecider()
	fd.set(policy.KindOpenURL, broker.Outcome{Verdict: broker.Pending, Request: 7, Reason: "open_urls = \"ask\""})
	ba := &blockingActions{block: make(chan struct{})}
	defer close(ba.block)
	a := startExecHost(t, Config{Ports: PortSet{}, OpenURL: OpenAllow, Actions: ba, Decide: fd})
	r := a.request(agentproto.Message{Op: agentproto.OpOpen, URL: "https://Example.COM:8443/a?b#c"})
	if r.OK || r.Reason != agentproto.ReasonPending || r.RequestID != 7 || !strings.Contains(r.Error, "pending #7") {
		t.Fatalf("got %+v", r)
	}
	if len(ba.opened) != 0 {
		t.Fatalf("opened %v", ba.opened)
	}
	fd.mu.Lock()
	got := fd.decided[0]
	fd.mu.Unlock()
	if got.Kind != policy.KindOpenURL || got.URL != "https://Example.COM:8443/a?b#c" || got.Origin != "https://example.com:8443" {
		t.Fatalf("decided %+v", got)
	}
	// Allowed, it opens at once, as checked.
	fd.set(policy.KindOpenURL, broker.Outcome{Verdict: broker.Allowed})
	if r := a.request(agentproto.Message{Op: agentproto.OpOpen, URL: "https://example.com/"}); !r.OK {
		t.Fatalf("allowed: %+v", r)
	}
	if len(ba.opened) != 1 || ba.opened[0] != "https://example.com/" {
		t.Fatalf("opened %v", ba.opened)
	}
	// What CheckURL refuses is never decided at all.
	fd.mu.Lock()
	n := len(fd.decided)
	fd.mu.Unlock()
	if r := a.request(agentproto.Message{Op: agentproto.OpOpen, URL: "file:///etc/passwd"}); r.OK {
		t.Fatalf("a file URL: %+v", r)
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.decided) != n {
		t.Fatal("a URL CheckURL refuses reached the Decider")
	}
}

// The outbound proxy dials only what the Decider allows, with the facts
// the host resolved: the name in lower case, every address it resolved
// to, this machine's own. A connection that asks is refused as pending,
// with the request's number, and nothing is dialled.
func TestEgressThroughDecider(t *testing.T) {
	fn := &fakeNet{echo: echoServer(t), names: map[string][]netip.Addr{
		"github.com": {netip.MustParseAddr("140.82.112.3"), netip.MustParseAddr("2606:50c0::1")},
		"git.corp":   {netip.MustParseAddr("10.1.2.3")},
	}}
	ports, _ := ParsePortsOf("egress_ports", "443")
	own := []netip.Addr{netip.MustParseAddr("192.0.2.9")}
	fd := newFakeDecider()
	a := egressLinked(t, Config{Decide: fd, Egress: &Egress{Ports: ports, Resolver: fn, Dial: fn.Dial,
		Own: func() ([]netip.Addr, error) { return own, nil }}})

	fd.set(policy.KindEgress, broker.Outcome{Verdict: broker.Pending, Request: 12, Reason: "git.corp:443: private"})
	r := a.connect("git.corp", 443)
	if r.OK || r.Reason != agentproto.EgressPending || r.RequestID != 12 || !strings.Contains(r.Error, "pending #12") {
		t.Fatalf("pending: %+v", r)
	}
	fd.set(policy.KindEgress, broker.Outcome{Verdict: broker.Denied, Reason: "git.corp:443: egress_deny names it"})
	if r := a.connect("git.corp", 443); r.OK || r.Reason != agentproto.EgressDenied || !strings.Contains(r.Error, "egress_deny") {
		t.Fatalf("denied: %+v", r)
	}
	fn.mu.Lock()
	if len(fn.dialled) != 0 {
		t.Fatalf("dialled %v", fn.dialled)
	}
	fn.mu.Unlock()
	// A port egress_ports refuses is refused before anything is resolved.
	if r := a.connect("github.com", 25); r.Reason != agentproto.EgressPort {
		t.Fatalf("port 25: %+v", r)
	}
	fd.set(policy.KindEgress, broker.Outcome{Verdict: broker.Allowed})
	if r := a.connect("GitHub.com.", 443); !r.OK {
		t.Fatalf("allowed: %+v", r)
	}
	if r := a.connect("140.82.112.3", 443); !r.OK {
		t.Fatalf("allowed literal: %+v", r)
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.decided) != 4 {
		t.Fatalf("decided %d times: %+v", len(fd.decided), fd.decided)
	}
	byName, literal := fd.decided[2], fd.decided[3]
	if byName.Host != "github.com" || byName.Literal || byName.Port != 443 || len(byName.Addrs) != 2 ||
		byName.Addrs[1] != netip.MustParseAddr("2606:50c0::1") || len(byName.Own) != 1 || byName.Own[0] != own[0] {
		t.Errorf("by name: %+v", byName)
	}
	if literal.Host != "140.82.112.3" || !literal.Literal || len(literal.Addrs) != 1 {
		t.Errorf("literal: %+v", literal)
	}
	fn.mu.Lock()
	defer fn.mu.Unlock()
	if len(fn.dialled) != 2 || fn.dialled[0] != "140.82.112.3:443" {
		t.Errorf("dialled %v", fn.dialled)
	}
}

// The hello offers the requests only with a broker behind the link, and
// host exec only when offered.
func TestHelloOffers(t *testing.T) {
	if a := startExecHost(t, Config{}); a.hello.Broker || a.hello.HostExec {
		t.Errorf("no broker, no host exec: %+v", a.hello)
	}
	_, cfg := execRoot(t)
	cfg.Decide = newFakeDecider()
	if a := startExecHost(t, cfg); !a.hello.Broker || !a.hello.HostExec {
		t.Errorf("broker and host exec: %+v", a.hello)
	}
}

// An agent of another protocol version is refused at its hello, and none
// of its requests is served: not half-served.
func TestOtherVersionRefused(t *testing.T) {
	for _, v := range []int{0, agentproto.Version + 1} {
		hr, aw := io.Pipe()
		ar, hw := io.Pipe()
		hs := agentproto.NewSession(hr, hw, true)
		as := agentproto.NewSession(ar, aw, false)
		fd := newFakeDecider()
		fd.set(policy.KindOpenURL, broker.Outcome{Verdict: broker.Allowed})
		fa := &fakeActions{}
		done := make(chan error, 1)
		go func() {
			done <- Run(hs, Config{Ports: PortSet{}, Actions: fa, Decide: fd, Log: log.New(io.Discard, "", 0)})
		}()
		<-as.Control() // the host's hello
		_ = as.Send(agentproto.Message{Type: agentproto.TypeHello, Version: v})
		_ = as.Send(agentproto.Message{Type: agentproto.TypeRequest, ID: 1, Op: agentproto.OpOpen, URL: "https://example.com/"})
		select {
		case err := <-done:
			if v == 0 && err == nil || v != 0 && (err == nil || !strings.Contains(err.Error(), "protocol version")) {
				t.Errorf("version %d: %v", v, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("version %d: served", v)
		}
		hs.Close()
		as.Close()
		fd.mu.Lock()
		if len(fd.decided) != 0 || len(fa.opened) != 0 {
			t.Errorf("version %d: decided %d, opened %v", v, len(fd.decided), fa.opened)
		}
		fd.mu.Unlock()
	}
}

// A request filed ahead of time is checked and bounded first, then filed
// with the facts the host resolved itself, never the agent's, and the
// verdict comes back with the request's number.
func TestBrokerRequest(t *testing.T) {
	fn := &fakeNet{names: map[string][]netip.Addr{"api.example.com": {netip.MustParseAddr("10.9.8.7")}}}
	ports, _ := ParsePortsOf("egress_ports", "443")
	fd := newFakeDecider()
	fd.set(policy.KindEgress, broker.Outcome{Verdict: broker.Pending, Request: 41, Reason: "private"})
	fd.set(policy.KindOpenURL, broker.Outcome{Verdict: broker.Allowed})
	own := []netip.Addr{netip.MustParseAddr("192.0.2.9")}
	a := startExecHost(t, Config{Decide: fd, Actions: &fakeActions{}, Egress: &Egress{Ports: ports, Resolver: fn,
		Own: func() ([]netip.Addr, error) { return own, nil }}})

	ask := &agentproto.Ask{Kind: "egress", Host: "API.example.com", Port: 443, Context: "the tests fetch a fixture",
		Options: []string{"allow for 8h", "deny"}, Recommendation: "allow", Meanwhile: "skipped that test", Session: "s1"}
	r := a.request(agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: ask})
	if r.OK || r.Verdict != agentproto.VerdictPending || r.RequestID != 41 || !strings.Contains(r.Error, "pending #41") {
		t.Fatalf("egress: %+v", r)
	}
	r = a.request(agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: &agentproto.Ask{Kind: "open_url", URL: "https://Docs.example.com/x"}})
	if !r.OK || r.Verdict != agentproto.VerdictAllowed {
		t.Fatalf("open_url: %+v", r)
	}
	fd.mu.Lock()
	if len(fd.filed) != 2 {
		t.Fatalf("filed %+v", fd.filed)
	}
	eg, u := fd.filed[0], fd.filed[1]
	fd.mu.Unlock()
	if eg.Request.Kind != policy.KindEgress || eg.Request.Host != "api.example.com" || len(eg.Request.Addrs) != 1 || eg.Request.Addrs[0] != netip.MustParseAddr("10.9.8.7") ||
		eg.Context != ask.Context || len(eg.Options) != 2 || eg.Recommendation != "allow" || eg.Meanwhile != ask.Meanwhile || eg.Session != "s1" {
		t.Errorf("egress filed as %+v", eg)
	}
	// This machine's own addresses go with it, so that the policy refuses
	// a name that resolves to one of them.
	if len(eg.Request.Own) != 1 || eg.Request.Own[0] != own[0] {
		t.Errorf("egress filed with own addresses %v, want %v", eg.Request.Own, own)
	}
	if u.Request.Origin != "https://docs.example.com" {
		t.Errorf("open_url filed as %+v", u)
	}

	for _, c := range []struct {
		name string
		ask  *agentproto.Ask
		want string
	}{
		{"none", nil, "names nothing"},
		{"options", &agentproto.Ask{Kind: "open_url", URL: "https://example.com/", Options: make([]string, broker.MaxOptions+1)}, "options"},
		{"long option", &agentproto.Ask{Kind: "open_url", URL: "https://example.com/", Options: []string{strings.Repeat("o", broker.MaxOption+1)}}, "option 1"},
		{"context", &agentproto.Ask{Kind: "open_url", URL: "https://example.com/", Context: strings.Repeat("c", broker.MaxContext+1)}, "context"},
		{"session", &agentproto.Ask{Kind: "open_url", URL: "https://example.com/", Session: strings.Repeat("s", broker.MaxSession+1)}, "session"},
		{"kind", &agentproto.Ask{Kind: "host_exec"}, "kind"},
		{"alias", &agentproto.Ask{Kind: "alias", Alias: "x"}, "aliases"},
		{"url", &agentproto.Ask{Kind: "open_url", URL: "javascript:alert(1)"}, "only http and https"},
		{"host", &agentproto.Ask{Kind: "egress", Host: "a b", Port: 443}, "not a host name"},
		{"port", &agentproto.Ask{Kind: "egress", Host: "example.com", Port: 25}, "egress_ports"},
	} {
		// The bucket refills: these come slower than requestEvery allows
		// otherwise.
		a2 := startExecHost(t, Config{Decide: fd, Actions: &fakeActions{}, Egress: &Egress{Ports: ports, Resolver: fn}})
		r := a2.request(agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: c.ask})
		if r.OK || r.Verdict != "" || !strings.Contains(r.Error, c.want) {
			t.Errorf("%s: %+v, want %q", c.name, r, c.want)
		}
	}
	fd.mu.Lock()
	if len(fd.filed) != 2 {
		t.Errorf("a refused request was filed: %+v", fd.filed[2:])
	}
	fd.mu.Unlock()

	// Without a broker, nothing is filed.
	if r := startExecHost(t, Config{}).request(agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: ask}); r.OK || !strings.Contains(r.Error, "files no requests") {
		t.Errorf("no broker: %+v", r)
	}
}

// Filing shares the request bucket: a burst past it is refused, and the
// rest is not filed.
func TestBrokerRequestRateLimited(t *testing.T) {
	fd := newFakeDecider()
	a := startExecHost(t, Config{Decide: fd, Actions: &fakeActions{}})
	refused := 0
	for range requestBurst + 3 {
		r := a.request(agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: &agentproto.Ask{Kind: "open_url", URL: "https://example.com/"}})
		if strings.Contains(r.Error, "too many") {
			refused++
		}
	}
	_, filed, _, _ := fd.count()
	if refused == 0 || filed+refused != requestBurst+3 {
		t.Fatalf("refused %d, filed %d", refused, filed)
	}
}

// A wait answers once a decision for its filter is newer than its
// cursor; one for another session does not end it.
func TestBrokerRequestsWait(t *testing.T) {
	fd := newFakeDecider()
	fd.add(1, "s1")
	fd.add(2, "s2")
	a := startExecHost(t, Config{Decide: fd, Actions: &fakeActions{}})
	now := a.request(agentproto.Message{Op: agentproto.OpBrokerRequests, Session: "s1"})
	if !now.OK || now.Cursor != 1 || len(now.Requests) != 1 || now.Requests[0].ID != 1 || now.Requests[0].State != broker.StatePending {
		t.Fatalf("list: %+v", now)
	}
	id := a.send(agentproto.Message{Op: agentproto.OpBrokerRequests, Session: "s1", Since: now.Cursor, Wait: 60})
	waitFor(t, func() bool { _, _, w, _ := fd.count(); return w == 1 })
	fd.decide(2, broker.StateAllowed) // another session's
	if r, ok := a.answer(id, 300*time.Millisecond); ok {
		t.Fatalf("another session's decision ended the wait: %+v", r)
	}
	fd.decide(1, broker.StateDenied)
	r, ok := a.answer(id, 5*time.Second)
	if !ok || !r.OK || r.Cursor != 3 || len(r.Requests) != 1 || r.Requests[0].ID != 1 || r.Requests[0].State != broker.StateDenied || r.Requests[0].Seq != 3 {
		t.Fatalf("after the decision: %v %+v", ok, r)
	}
	// Run out, it answers with what there is.
	start := time.Now()
	r = a.request(agentproto.Message{Op: agentproto.OpBrokerRequests, Session: "s1", Since: 3, Wait: 1})
	if !r.OK || len(r.Requests) != 0 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("timed out: %+v after %v", r, time.Since(start))
	}
}

// At most maxWaits wait on one link, whatever sessions they claim; the
// next is refused at once, and while they wait every other request is
// answered. They end with the session.
func TestBrokerRequestsWaitsBounded(t *testing.T) {
	fd := newFakeDecider()
	fa := &fakeActions{}
	a := startExecHost(t, Config{Decide: fd, Actions: fa, OpenURL: OpenAllow})
	for i := range maxWaits {
		a.send(agentproto.Message{Op: agentproto.OpBrokerRequests, Session: fmt.Sprintf("s%d", i), Since: 1, Wait: agentproto.MaxWait})
	}
	waitFor(t, func() bool { _, _, w, _ := fd.count(); return w == maxWaits })
	r := a.request(agentproto.Message{Op: agentproto.OpBrokerRequests, Session: "another", Since: 1, Wait: 10})
	if r.OK || !strings.Contains(r.Error, "too many waits") {
		t.Fatalf("one wait too many: %+v", r)
	}
	start := time.Now()
	if r := a.request(agentproto.Message{Op: agentproto.OpNotify, Text: "still here"}); !r.OK {
		t.Fatalf("notify: %+v", r)
	}
	if r := a.request(agentproto.Message{Op: agentproto.OpBrokerRequests}); !r.OK {
		t.Fatalf("list: %+v", r)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("other requests took %v while the waits waited", d)
	}
	a.sess.Close()
	waitFor(t, func() bool { _, _, w, _ := fd.count(); return w == 0 })
}

// A wait is clamped to MaxWait, and listing has its own bucket.
func TestBrokerRequestsBounds(t *testing.T) {
	fd := newFakeDecider()
	a := startExecHost(t, Config{Decide: fd, Actions: &fakeActions{}})
	if r := a.request(agentproto.Message{Op: agentproto.OpBrokerRequests, Session: strings.Repeat("s", broker.MaxSession+1)}); r.OK {
		t.Errorf("a long session: %+v", r)
	}
	limited := false
	for range listBurst + 5 {
		if r := a.request(agentproto.Message{Op: agentproto.OpBrokerRequests}); strings.Contains(r.Error, "too many requests") {
			limited = true
		}
	}
	if !limited {
		t.Error("listing is not rate-limited")
	}
	if r := startExecHost(t, Config{}).request(agentproto.Message{Op: agentproto.OpBrokerRequests}); r.OK {
		t.Errorf("no broker: %+v", r)
	}
}

// What one response lists fits one control message, newest first. The
// decisions after the cursor come before pending requests, the oldest
// first; when some are left out, the cursor goes back to just before the
// earliest left out, so a wait from it is told of them next, and of
// nothing it already had.
func TestListedFits(t *testing.T) {
	big := func(id, seq uint64, state string) broker.RequestInfo {
		return broker.RequestInfo{ID: id, Kind: policy.KindEgress, State: state, Seq: seq,
			Effect: strings.Repeat("e", 4000), Reason: strings.Repeat("r", 4000), Session: strings.Repeat("s", 100)}
	}
	var reqs []broker.RequestInfo
	for i := 300; i >= 1; i-- {
		reqs = append(reqs, big(uint64(i), uint64(i+10), broker.StateDenied))
	}
	var r agentproto.Message
	listed(&r, reqs, 400, 5)
	r.Type = agentproto.TypeResponse
	b, err := agentproto.Encode(r)
	if err != nil || len(b) > agentproto.MaxPayload {
		t.Fatalf("%d bytes, %v", len(b), err)
	}
	n := len(r.Requests)
	if n == 0 || n >= len(reqs) || r.Requests[0].ID != uint64(n) || r.Requests[n-1].ID != 1 {
		t.Fatalf("listed %d, from %d to %d", n, r.Requests[0].ID, r.Requests[n-1].ID)
	}
	// The earliest left out is request n+1, decided at seq n+11.
	if r.Cursor != uint64(n+10) {
		t.Fatalf("cursor %d, %d listed", r.Cursor, n)
	}
	for _, q := range r.Requests {
		if len(q.Effect) > maxListedEffect+len("…") || len(q.Reason) > maxListedReason+len("…") {
			t.Fatalf("not clipped: %d, %d", len(q.Effect), len(q.Reason))
		}
	}
	// The next wait, from that cursor, gets the rest and nothing again.
	var next agentproto.Message
	listed(&next, reqs[:len(reqs)-n], 400, r.Cursor)
	if next.Requests[len(next.Requests)-1].ID != uint64(n+1) {
		t.Fatalf("next starts at %d", next.Requests[len(next.Requests)-1].ID)
	}
	// Pending requests filling the budget never crowd a decision out.
	var mixed []broker.RequestInfo
	for i := 200; i >= 2; i-- {
		mixed = append(mixed, big(uint64(i), 0, broker.StatePending))
	}
	mixed = append(mixed, big(1, 50, broker.StateAllowed))
	var m agentproto.Message
	listed(&m, mixed, 50, 49)
	if got := m.Requests[len(m.Requests)-1]; got.ID != 1 || got.State != broker.StateAllowed || m.Cursor != 50 {
		t.Fatalf("the decision was left out: last %+v, cursor %d", got, m.Cursor)
	}
	// All of it fits: the cursor stays.
	var small agentproto.Message
	listed(&small, reqs[:3], 400, 5)
	if small.Cursor != 400 || len(small.Requests) != 3 {
		t.Fatalf("%+v", small)
	}
	// At most MaxRequests, whatever their size.
	var many []broker.RequestInfo
	for i := range agentproto.MaxRequests + 50 {
		many = append(many, broker.RequestInfo{ID: uint64(i), State: broker.StatePending})
	}
	var mm agentproto.Message
	listed(&mm, many, 9, 0)
	if len(mm.Requests) != agentproto.MaxRequests || mm.Cursor != 9 {
		t.Fatalf("%d listed, cursor %d", len(mm.Requests), mm.Cursor)
	}
}

// The agent's count of sessions with caboose's mod is passed on when it
// changes; one that is no count is dropped.
func TestModsReceived(t *testing.T) {
	var mu sync.Mutex
	var got []agentproto.ModsInfo
	a := startExecHost(t, Config{Actions: &fakeActions{}, Mods: func(m agentproto.ModsInfo) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}})
	for _, m := range []*agentproto.ModsInfo{
		{Sessions: 2, WithMod: 1}, {Sessions: 2, WithMod: 1}, nil, {Sessions: -1}, {Sessions: 1, WithMod: 2},
		{Sessions: 1 << 30, WithMod: 0}, {Sessions: 3, WithMod: 3},
	} {
		if err := a.sess.Send(agentproto.Message{Type: agentproto.TypeMods, Mods: m}); err != nil {
			t.Fatal(err)
		}
	}
	// A request after them is answered after they were read.
	a.request(agentproto.Message{Op: agentproto.OpNotify, Text: "x"})
	mu.Lock()
	defer mu.Unlock()
	want := []agentproto.ModsInfo{{Sessions: 2, WithMod: 1}, {Sessions: 3, WithMod: 3}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// waitFor waits until cond holds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}

// testOwner owns its environment while its file holds a lock.
type testOwner struct{}

func (testOwner) Owns() error { return nil }

// realBroker is a broker in a temporary CABOOSE_HOME, on c.
func realBroker(t *testing.T, c config.Config, opener broker.Opener) *broker.Broker {
	t.Helper()
	home := t.TempDir()
	b, err := broker.Open(broker.Options{Home: home, Env: "t", Owner: testOwner{},
		Source:   func() (broker.Settings, error) { cc := c; return broker.Settings{Config: &cc}, nil },
		Writable: func() ([]string, error) { return []string{"/"}, nil }, Opener: opener})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// Through a real broker: what asks is filed and refused for now; nothing
// the sandbox sends answers it or grants anything -- not filing it again
// with words that say "allow", not listing, not opening it again; only an
// answer on the host does, and then the very next try is allowed. A grant
// by name, made at run time, never lets that name through once it
// resolves to a private address it does not hold.
func TestRealBroker(t *testing.T) {
	fn := &fakeNet{echo: echoServer(t), names: map[string][]netip.Addr{
		"api.example.com": {netip.MustParseAddr("140.82.112.9")},
	}}
	ports, _ := ParsePortsOf("egress_ports", "443")
	fa := &fakeActions{}
	b := realBroker(t, config.Config{Trust: config.TrustHostile, OpenURLs: "ask", Isolation: config.KindVM, Egress: true, EgressPorts: "443"}, fa)
	link := b.Link()
	a := egressLinked(t, Config{Decide: link, Actions: fa, Egress: &Egress{Ports: ports, Resolver: fn, Dial: fn.Dial,
		Own: func() ([]netip.Addr, error) { return nil, nil }}})

	r := a.connect("api.example.com", 443)
	if r.OK || r.Reason != agentproto.EgressPending || r.RequestID == 0 {
		t.Fatalf("hostile, unnamed: %+v", r)
	}
	id := r.RequestID
	// The sandbox's every way of saying yes.
	a.next++
	_ = a.sess.Send(agentproto.Message{Type: agentproto.TypeRequest, ID: a.next, Op: agentproto.OpBrokerRequest,
		Ask: &agentproto.Ask{Kind: "egress", Host: "api.example.com", Port: 443, Recommendation: "allow", Options: []string{"allow always"}}})
	_ = a.sess.Send(agentproto.Message{Type: agentproto.TypeResponse, ID: id, OK: true, Verdict: agentproto.VerdictAllowed})
	_ = a.sess.Send(agentproto.Message{Type: agentproto.TypeRequest, ID: 999, Op: agentproto.OpBrokerRequests, All: true})
	if r := a.connect("api.example.com", 443); r.OK || r.RequestID != id {
		t.Fatalf("still pending: %+v", r)
	}
	if g, _ := b.Grants(); len(g) != 0 {
		t.Fatalf("grants from the sandbox: %+v", g)
	}
	if q, _ := b.Request(id); q.State != broker.StatePending {
		t.Fatalf("request %+v", q)
	}
	// The host answers: the next connection goes through.
	if _, err := b.Answer(id, broker.Reply{Verdict: broker.ReplyAllow, Scope: broker.ScopeHours, Hours: 1, By: broker.ByCLI}); err != nil {
		t.Fatal(err)
	}
	if r := a.connect("api.example.com", 443); !r.OK {
		t.Fatalf("after the answer: %+v", r)
	} else if st, err := a.sess.Accept(); err == nil {
		st.Close()
	}
	// The name now resolves to a private address: the grant, made for a
	// public one, does not reach it.
	fn.mu.Lock()
	fn.names["api.example.com"] = []netip.Addr{netip.MustParseAddr("10.0.0.5")}
	fn.mu.Unlock()
	if r := a.connect("api.example.com", 443); r.OK || r.Reason != agentproto.EgressPending {
		t.Fatalf("rebound to a private address: %+v", r)
	}
	// Loopback never, grant or not.
	fn.mu.Lock()
	fn.names["api.example.com"] = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	fn.mu.Unlock()
	if r := a.connect("api.example.com", 443); r.OK || r.Reason != agentproto.EgressDenied {
		t.Fatalf("rebound to loopback: %+v", r)
	}

	// A URL: pending at once, opened by the broker once allowed, never
	// by the link.
	a.next++
	_ = a.sess.Send(agentproto.Message{Type: agentproto.TypeRequest, ID: a.next, Op: agentproto.OpOpen, URL: "https://example.com/doc"})
	var u agentproto.Message
	for {
		u = a.control()
		if u.Type == agentproto.TypeResponse && u.ID == a.next {
			break
		}
	}
	if u.OK || u.Reason != agentproto.ReasonPending || u.RequestID == 0 {
		t.Fatalf("URL: %+v", u)
	}
	fa.mu.Lock()
	if len(fa.opened) != 0 {
		t.Fatalf("opened before the answer: %v", fa.opened)
	}
	fa.mu.Unlock()
	if _, err := b.Answer(u.RequestID, broker.Reply{Verdict: broker.ReplyAllow, Scope: broker.ScopeOnce, By: broker.ByDialog}); err != nil {
		t.Fatal(err)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.opened) != 1 || fa.opened[0] != "https://example.com/doc" {
		t.Fatalf("opened %v", fa.opened)
	}
}

// The stream header a pending refusal never has: no stream opens for a
// connection refused for now.
func TestEgressPendingOpensNoStream(t *testing.T) {
	fd := newFakeDecider()
	fd.set(policy.KindEgress, broker.Outcome{Verdict: broker.Pending, Request: 2})
	ports, _ := ParsePortsOf("egress_ports", "443")
	fn := &fakeNet{echo: echoServer(t), names: map[string][]netip.Addr{"x.example": {netip.MustParseAddr("140.82.112.3")}}}
	a := egressLinked(t, Config{Decide: fd, Egress: &Egress{Ports: ports, Resolver: fn, Dial: fn.Dial}})
	_ = a.connect("x.example", 443)
	accepted := make(chan struct{})
	go func() {
		if st, err := a.sess.Accept(); err == nil {
			var h agentproto.StreamHeader
			_ = json.Unmarshal(st.Header(), &h)
			close(accepted)
		}
	}()
	select {
	case <-accepted:
		t.Fatal("a stream opened for a pending connection")
	case <-time.After(200 * time.Millisecond):
	}
}

// The test binary's own lock on a temporary file, for helpers that need
// a held flock.
func heldLock(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "link.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// A broker owned through the link's link.lock serves only while it is
// held.
func TestBrokerOwnedByLock(t *testing.T) {
	f := heldLock(t)
	if err := broker.LockOwner(f).Owns(); err != nil {
		t.Fatalf("the holder does not own it: %v", err)
	}
	other, err := os.OpenFile(f.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := broker.LockOwner(other).Owns(); err == nil {
		t.Fatal("another open of link.lock owns it")
	}
}
