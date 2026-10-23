package menubar

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// loginOf is the login item of m, whichever state it shows.
func loginOf(m Menu) *Item {
	for _, id := range []string{"login:on", "login:off", "login:approve"} {
		if it := find(m, id); it != nil {
			return it
		}
	}
	return nil
}

// selectLogin selects the login item the last menu shows.
func (r *rig) selectLogin() {
	r.t.Helper()
	it := loginOf(r.ui.lastMenu())
	if it == nil {
		r.t.Fatalf("no login item in\n%s", Dump(r.ui.lastMenu()))
	}
	r.selectItem(it.ID)
}

// editStatus changes the status the fake daemon answers from now on.
func (r *rig) editStatus(f func(*daemonproto.StatusResult)) {
	r.d.mu.Lock()
	prev := r.d.handlers[daemonproto.MethodStatus]
	r.d.mu.Unlock()
	r.d.handle(daemonproto.MethodStatus, func(p json.RawMessage) (any, error) {
		v, err := prev(p)
		if err != nil {
			return v, err
		}
		st := v.(daemonproto.StatusResult)
		st.Envs = append([]daemonproto.EnvStatus(nil), st.Envs...)
		f(&st)
		return st, nil
	})
}

// closeThenSelect is AppKit's order: the menu's close is posted before
// the action of the item chosen in it.
func (r *rig) closeThenSelect(shown Menu, id string) {
	r.t.Helper()
	if find(shown, id) == nil {
		r.t.Fatalf("no item %s in the menu shown:\n%s", id, Dump(shown))
	}
	r.do(func(e Events) { e.MenuClosed() })
	r.do(func(e Events) { e.Selected(id) })
}

// openAndChange opens the menu, lets f change what the daemon says, and
// lets the core see it while the menu is open; it returns the menu shown.
func (r *rig) openAndChange(f func()) Menu {
	r.t.Helper()
	shown := r.ui.lastMenu()
	r.do(func(e Events) { e.MenuOpened() })
	f()
	r.advance(RequestsEvery + PollEvery)
	if !reflect.DeepEqual(r.ui.lastMenu(), shown) {
		r.t.Fatal("the menu changed while open")
	}
	return shown
}

// TestSelectAfterCloseStillApplies: an item that left the menu while it
// was open, though what it does still applies, does what the user saw.
func TestSelectAfterCloseStillApplies(t *testing.T) {
	r := newRig(t, nil)
	s := scripted(r)
	r.start()
	r.advance(PollEvery)
	shown := r.openAndChange(func() {
		// Five newer requests push #6 out of the twenty listed.
		s.mu.Lock()
		for i := 26; i <= 30; i++ {
			id := uint64(i)
			v := view(id, "egress", fmt.Sprintf("egress:new%d.example.com", i), "connect", t0.Add(time.Duration(i)*time.Minute))
			s.views["dev"] = append([]daemonproto.RequestView{v}, s.views["dev"]...)
			s.offers[id] = []daemonproto.Offer{{Scope: daemonproto.ScopeOnce, Effect: "allow once"}}
		}
		s.mu.Unlock()
		r.editStatus(func(st *daemonproto.StatusResult) { st.Envs[0].Pending = 30 })
	})
	r.closeThenSelect(shown, "req:dev:6:once")
	if find(r.ui.lastMenu(), "req:dev:6:once") != nil {
		t.Fatalf("the test's menu still lists #6:\n%s", Dump(r.ui.lastMenu()))
	}
	cfs := r.ui.snapshot().confirm
	if len(cfs) != 1 || cfs[0].Message != "Allow #6 once?" {
		t.Errorf("confirmations %+v, want the one for #6", cfs)
	}
	if len(r.d.callsOf(daemonproto.MethodAnswer)) != 0 {
		t.Error("answered without the confirmation")
	}
}

// TestSelectAfterCloseGone: an item whose request was decided, or whose
// sandbox stopped, while the menu was open says so, and does nothing.
func TestSelectAfterCloseGone(t *testing.T) {
	r := newRig(t, nil)
	s := scripted(r)
	r.start()
	r.advance(PollEvery)

	// #25 is answered elsewhere while the menu is open.
	shown := r.openAndChange(func() {
		s.mu.Lock()
		s.gone[25] = true
		s.views["dev"] = s.views["dev"][1:]
		s.mu.Unlock()
		r.editStatus(func(st *daemonproto.StatusResult) { st.Envs[0].Pending = 24 })
	})
	r.closeThenSelect(shown, "req:dev:25:once")
	if findTitle(r.ui.lastMenu(), "#25 is no longer pending") == nil {
		t.Errorf("no line for the decided request:\n%s", Dump(r.ui.lastMenu()))
	}

	// Still listed by the daemon, but decided: who decided is said.
	s.mu.Lock()
	s.gone[24] = true
	s.mu.Unlock()
	r.selectItem("req:dev:24:deny")
	if findTitle(r.ui.lastMenu(), "#24 was already answered by cli") == nil {
		t.Errorf("no line for #24:\n%s", Dump(r.ui.lastMenu()))
	}

	// dev stops while the menu is open: its stop, restart and ports go.
	shown = r.openAndChange(func() {
		r.editStatus(func(st *daemonproto.StatusResult) { st.Envs[0].Sandbox = daemonproto.SandboxStopped })
	})
	r.closeThenSelect(shown, "stop:dev")
	r.do(func(e Events) { e.Selected("port:dev:5173") })
	if findTitle(r.ui.lastMenu(), "dev: not sent, the sandbox is no longer running") == nil ||
		findTitle(r.ui.lastMenu(), "dev: localhost:5173 is no longer listening") == nil {
		t.Errorf("no lines for the stopped sandbox:\n%s", Dump(r.ui.lastMenu()))
	}
	snap := r.ui.snapshot()
	if len(snap.confirm) != 0 || len(snap.opened) != 0 || len(r.d.callsOf(daemonproto.MethodAnswer)) != 0 {
		t.Errorf("acted on what no longer applies: confirm %+v, opened %v", snap.confirm, snap.opened)
	}
	// An ID no menu ever held does nothing.
	r.do(func(e Events) { e.Selected("stop:nowhere") })
	if n := len(r.ui.snapshot().confirm); n != 0 {
		t.Errorf("%d confirmations for an unknown item", n)
	}
}

// TestSelectAfterCloseLogin: the login item does what its state said,
// never the opposite, when the state moved since.
func TestSelectAfterCloseLogin(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	shown := r.ui.lastMenu()
	if loginOf(shown).ID != "login:on" {
		t.Fatalf("login item %+v", loginOf(shown))
	}
	r.selectLogin() // on
	r.do(func(e Events) { e.Selected("login:on") })
	if got := fmt.Sprint(r.ui.setLog); got != "[true]" {
		t.Errorf("set %s: a stale \"turn on\" turned it off", got)
	}
	if findTitle(r.ui.lastMenu(), "Open at login changed meanwhile: nothing done") == nil {
		t.Errorf("no line:\n%s", Dump(r.ui.lastMenu()))
	}
}

// TestIDsStandForOneAction: whatever the menu is built from, an item ID
// stands for one action, which is what lets a selection made in an
// older menu resolve against a newer one's table.
func TestIDsStandForOneAction(t *testing.T) {
	seen := map[string]action{}
	offers := [][]daemonproto.Offer{nil, {{Scope: daemonproto.ScopeOnce}},
		{{Scope: daemonproto.ScopeHours, HoursMin: 1, HoursMax: 8}}, allOffers()}
	for _, login := range []LoginState{LoginUnavailable, LoginOff, LoginOn, LoginNeedsApproval} {
		for _, sb := range []string{daemonproto.SandboxRunning, daemonproto.SandboxStopped} {
			for _, job := range []*daemonproto.JobInfo{nil, {Kind: daemonproto.JobRestart, State: daemonproto.JobRunning}} {
				for oi, o := range offers {
					for _, listening := range []bool{false, true} {
						st := &daemonproto.StatusResult{
							Daemon: daemonproto.DaemonInfo{Dir: "/d", Web: &daemonproto.WebState{Listening: listening}},
							Envs: []daemonproto.EnvStatus{{Name: "dev", Sandbox: sb, Pending: 2, Job: job},
								{Name: "ops", Sandbox: daemonproto.SandboxRunning}},
							Attention: make([]daemonproto.AttentionItem, MaxAttentionLines+oi),
						}
						var reqs []reqEntry
						for _, env := range []string{"dev", "ops"} {
							for id := uint64(1); id <= 2; id++ {
								reqs = append(reqs, reqEntry{env: env, view: view(id, "egress", "k", "e", t0),
									offers: &daemonproto.RequestResult{Offers: o}})
							}
						}
						m := model{now: t0, status: st, requests: reqs, login: login,
							details: map[string]*daemonproto.EnvDetailsResult{
								"dev": {Ports: []daemonproto.PortDetail{{Port: 3000}, {Port: 5173 + oi}}},
								"ops": {Ports: []daemonproto.PortDetail{{Port: 3000}}}}}
						_, acts := build(m)
						for id, a := range acts {
							if prev, ok := seen[id]; ok && prev != a {
								t.Fatalf("%s stands for %+v and %+v", id, prev, a)
							}
							seen[id] = a
						}
					}
				}
			}
		}
	}
	for _, want := range []string{"req:dev:1:once", "req:ops:2:hours:8", "req:dev:1:always", "stop:dev", "port:ops:3000", "login:off", "login:approve"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("no %s among %s", want, strings.Join(keys(seen), " "))
		}
	}
}

func keys(m map[string]action) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
