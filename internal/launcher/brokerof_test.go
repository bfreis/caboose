package launcher

import (
	"testing"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// Without the tests' hook, brokerOf serves an environment's broker only
// from that environment's own running link: never another environment's,
// whose link runs in this daemon too, and never one of a link that has no
// broker or is not running.
func TestBrokerOfServesOnlyTheEnvironmentsOwnLink(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	w.l.brokerHook = nil
	w.l.mu.Lock()
	w.l.envs["alpha"] = &envLink{name: "alpha", cur: &linkInstance{runner: &linkRunner{broker: w.b}}}
	w.l.envs["beta"] = &envLink{name: "beta"}                                              // no link runs
	w.l.envs["gamma"] = &envLink{name: "gamma", cur: &linkInstance{runner: &linkRunner{}}} // a link with no broker
	w.l.mu.Unlock()
	// Taken out again before the DaemonLinks close (cleanups run last
	// first): they are no real links to stop.
	t.Cleanup(func() {
		w.l.mu.Lock()
		delete(w.l.envs, "alpha")
		delete(w.l.envs, "beta")
		delete(w.l.envs, "gamma")
		w.l.mu.Unlock()
	})

	h, err := w.l.brokerOf("alpha")
	if err != nil || h == nil || h.b != w.b {
		t.Fatalf("alpha: %v, %v", h, err)
	}
	for _, env := range []string{"beta", "gamma", "delta"} {
		if h, err := w.l.brokerOf(env); err == nil || kindOf(err) != daemonproto.KindNotOwner {
			t.Errorf("%s was served a broker (%v): %v", env, h, err)
		}
		if _, err := w.l.Call(t.Context(), daemonproto.ClientCLI, daemonproto.MethodGrants, env, []byte(`{"env":"`+env+`"}`)); kindOf(err) != daemonproto.KindNotOwner {
			t.Errorf("%s: grants answered: %v", env, err)
		}
	}
	if _, err := w.l.Call(t.Context(), daemonproto.ClientCLI, daemonproto.MethodGrants, "alpha", []byte(`{"env":"alpha"}`)); err != nil {
		t.Errorf("alpha's own grants: %v", err)
	}
}
