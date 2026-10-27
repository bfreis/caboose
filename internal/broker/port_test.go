package broker

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
)

func (f *fixture) grantCount() int {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	return len(f.b.st.grants.Grants)
}

func (f *fixture) requestCount() int {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	return len(f.b.st.reqs.Requests)
}

// A listening port the policy asks about is the link's scan, not the
// sandbox's request: Decide answers ask:port and files nothing, logs
// nothing, however often it is asked. Only File files.
func TestPortDecideNeverFiles(t *testing.T) {
	f := newFixture(t)
	for range 20 {
		for _, r := range []policy.Request{{Kind: policy.KindPort, Port: 4321}, portReq(4321)} {
			o := f.decide(r)
			if o.Verdict != Denied || o.Rule != policy.RuleAskPort || o.Request != 0 || !strings.Contains(o.Reason, "caboose-agent request port --port 4321") {
				t.Fatalf("%+v: %+v", r, o)
			}
		}
	}
	if n := f.requestCount(); n != 0 {
		t.Errorf("%d requests filed", n)
	}
	if log := string(f.read(decisionsFile)); strings.Contains(log, "ask:port") || strings.Contains(log, "port-unchecked") {
		t.Errorf("decisions.log: %q", log)
	}
	// A port the policy would allow once probed is denied unlogged too,
	// until it is.
	f.src.set(func(c *config.Config) { c.ForwardPorts = "4400" })
	if err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if o := f.decide(policy.Request{Kind: policy.KindPort, Port: 4400}); o.Verdict != Denied || o.Rule != policy.RulePortUnchecked || o.Request != 0 {
			t.Fatalf("unchecked: %+v", o)
		}
	}
	if log := string(f.read(decisionsFile)); strings.Contains(log, "port-unchecked") {
		t.Errorf("decisions.log: %q", log)
	}
	// The host's deny of a port it will not forward is logged as any is.
	if o := f.decide(policy.Request{Kind: policy.KindPort, Port: 7171, HostChecked: true}); o.Verdict != Denied || o.Rule != "deny:ui_port" {
		t.Fatalf("ui port: %+v", o)
	}
	if !strings.Contains(string(f.read(decisionsFile)), "deny:ui_port") {
		t.Error("a ui port deny is not in decisions.log")
	}

	o, err := f.b.File(Ask{Request: portReq(4321)})
	if err != nil {
		t.Fatal(err)
	}
	mustPending(t, o)
	if o.Rule != policy.RuleAskPort {
		t.Errorf("filed with rule %q", o.Rule)
	}
}

// File does not use up a port's once grant: the link does, when it
// opens the forward.
func TestPortFileOnceGrant(t *testing.T) {
	f := newFixture(t)
	o, err := f.b.File(Ask{Request: portReq(4321)})
	if err != nil {
		t.Fatal(err)
	}
	id := mustPending(t, o)
	_, offers, _, err := f.b.RequestOffers(id)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("within %d minutes", int(OnceTTL.Minutes())); len(offers) == 0 || offers[0].Scope != ScopeOnce || !strings.Contains(offers[0].Effect, want) {
		t.Errorf("once offer lacks %q: %+v", want, offers)
	}
	f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeOnce})
	if f.grantCount() != 1 {
		t.Fatalf("%d grants after a once answer", f.grantCount())
	}
	for range 2 {
		o, err := f.b.File(Ask{Request: portReq(4321)})
		if err != nil {
			t.Fatal(err)
		}
		if o.Verdict != Allowed || f.grantCount() != 1 {
			t.Fatalf("File with a once grant: %+v, %d grants", o, f.grantCount())
		}
	}
	// The link's Decide of the checked port uses it.
	if o := f.decide(portReq(4321)); o.Verdict != Allowed || !strings.HasPrefix(o.Rule, "grant:") || !o.Until.IsZero() {
		t.Fatalf("Decide: %+v", o)
	}
	if f.grantCount() != 0 {
		t.Fatal("the once grant is still there")
	}
	if o := f.decide(portReq(4321)); o.Verdict != Denied || o.Rule != policy.RuleAskPort {
		t.Fatalf("after use: %+v", o)
	}
	// The grant was for the port asked about, not another.
	f.answer(mustPending(t, mustFile(t, f, portReq(4322))), Reply{Verdict: ReplyAllow, Scope: ScopeOnce})
	if o := f.decide(portReq(4321)); o.Verdict != Denied {
		t.Fatalf("another port's grant: %+v", o)
	}
}

func mustFile(t *testing.T, f *fixture, r policy.Request) Outcome {
	t.Helper()
	o, err := f.b.File(Ask{Request: r})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// A timed grant's end is in the Outcome, for the link to close the
// forward at.
func TestPortHoursUntil(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, mustFile(t, f, portReq(4321)))
	f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeHours, Hours: 2})
	want := f.clock.Now().Add(2 * time.Hour)
	for range 2 {
		o := f.decide(portReq(4321))
		if o.Verdict != Allowed || !o.Until.Equal(want) {
			t.Fatalf("%+v, want until %v", o, want)
		}
	}
	// Unchecked, the grant is not enough.
	if o := f.decide(policy.Request{Kind: policy.KindPort, Port: 4321}); o.Verdict != Denied || o.Rule != policy.RulePortUnchecked {
		t.Fatalf("unchecked: %+v", o)
	}
	// A program on the host beats the grant.
	if o := f.decide(policy.Request{Kind: policy.KindPort, Port: 4321, HostChecked: true, HostListens: true}); o.Verdict != Denied || o.Rule != "deny:port-host-listens" {
		t.Fatalf("host listens: %+v", o)
	}
	f.clock.Add(2*time.Hour + time.Second)
	if o := f.decide(portReq(4321)); o.Verdict != Denied || o.Rule != policy.RuleAskPort {
		t.Fatalf("after expiry: %+v", o)
	}
	// forward_ports allows without a grant, and with no end.
	f.src.set(func(c *config.Config) { c.ForwardPorts = "4321" })
	if err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	if o := f.decide(portReq(4321)); o.Verdict != Allowed || o.Rule != "allow:forward_ports" || !o.Until.IsZero() {
		t.Fatalf("forward_ports: %+v", o)
	}
}

// An "always" answer appends the port to forward_ports, writing out the
// defaults when the file sets none, and keeps what the file sets.
func TestAlwaysPort(t *testing.T) {
	f := newFixture(t)
	f.src.set(func(c *config.Config) { c.ForwardPorts = config.DefaultForwardPorts })
	if err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	id := mustPending(t, mustFile(t, f, portReq(4321)))
	o := f.alwaysOffer(id)
	if want := "link.forward_ports = [3000-3999 5173 8000-8999 32768-60999 4321]"; !strings.Contains(o.Diff, want) {
		t.Fatalf("diff %q lacks %q", o.Diff, want)
	}
	if !strings.Contains(o.Effect, "adds 4321 to forward_ports in [link]") {
		t.Errorf("effect %q", o.Effect)
	}
	f.answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeAlways, Confirm: o.DiffHash})
	if o := f.decide(portReq(4321)); o.Verdict != Allowed || o.Rule != "allow:forward_ports" {
		t.Fatalf("after always: %+v", o)
	}
	if o := f.decide(portReq(3000)); o.Verdict != Allowed {
		t.Fatalf("a default port after always: %+v", o)
	}

	// "none" becomes the one port.
	f.src.set(func(c *config.Config) { c.ForwardPorts = "none" })
	if err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	id = mustPending(t, mustFile(t, f, portReq(4400)))
	o = f.alwaysOffer(id)
	if !strings.Contains(o.Diff, "link.forward_ports = [4400]") {
		t.Fatalf("diff %q", o.Diff)
	}
}

// A port below 1024, a web UI port and a port already forwarded are not
// added to forward_ports by an "always" answer.
func TestAlwaysPortRefused(t *testing.T) {
	f := newFixture(t)
	f.src.set(func(c *config.Config) { c.ForwardPorts = "4321" })
	if err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	for _, port := range []int{80, 7171, 4321} {
		q := &reqRec{Kind: policy.KindPort, Port: port}
		if _, err := f.b.alwaysEdit(q, Settings{Config: &config.Config{ForwardPorts: "4321"}}); err == nil {
			t.Errorf("port %d: an always edit was made", port)
		}
	}
}
