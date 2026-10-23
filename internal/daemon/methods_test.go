package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// Every method of the table, written out. A method added to the table
// fails this test until it is listed here.
func TestMethodTable(t *testing.T) {
	want := []string{
		daemonproto.MethodPing, daemonproto.MethodStatus, daemonproto.MethodEnsureLink, daemonproto.MethodRestartLink,
		daemonproto.MethodStopLink, daemonproto.MethodStop, daemonproto.MethodReplace,
		daemonproto.MethodRequests, daemonproto.MethodRequest, daemonproto.MethodAnswer, daemonproto.MethodGrants,
		daemonproto.MethodRevoke, daemonproto.MethodPermissions, daemonproto.MethodCheckpoints, daemonproto.MethodWritable,
	}
	// Served to a client of a newer protocol, which is to replace the
	// daemon: these alone.
	newer := []string{daemonproto.MethodPing, daemonproto.MethodStatus, daemonproto.MethodStop, daemonproto.MethodReplace}
	s := &server{}
	got := map[string]bool{}
	for _, m := range s.methods() {
		if got[m.name] {
			t.Errorf("%s is in the table twice", m.name)
		}
		got[m.name] = true
		if !slices.Contains(want, m.name) {
			t.Errorf("%s is in the table but not in this list", m.name)
		}
		if m.newer != slices.Contains(newer, m.name) {
			t.Errorf("%s: served to a newer protocol is %v", m.name, m.newer)
		}
		if m.run == nil {
			t.Errorf("%s has no run", m.name)
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("%s is listed but not in the table", name)
		}
	}
	if got[daemonproto.MethodSubscribe] {
		t.Error("subscribe is reserved: not in the table")
	}
}

// counting services: each records that it was reached.
type countingLinks struct {
	noLinks
	mu    sync.Mutex
	calls []string
}

func (l *countingLinks) Serve(_ context.Context, method, env string) (*daemonproto.LinkResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, method)
	return &daemonproto.LinkResult{Action: "started", Env: daemonproto.EnvStatus{Name: env}}, nil
}

func (l *countingLinks) served() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

func (l *countingLinks) Envs() []daemonproto.EnvStatus {
	return []daemonproto.EnvStatus{{Name: "alpha", Pending: 2}}
}

// paramsFor are valid params for each method of the table.
func paramsFor(name string) json.RawMessage {
	var p any
	switch name {
	case daemonproto.MethodReplace:
		p = &daemonproto.ReplaceParams{Version: "v9.9.9"}
	case daemonproto.MethodEnsureLink, daemonproto.MethodRestartLink, daemonproto.MethodStopLink, daemonproto.MethodCheckpoints:
		p = &daemonproto.EnvParams{Env: "alpha"}
	case daemonproto.MethodPing, daemonproto.MethodStatus, daemonproto.MethodStop, daemonproto.MethodWritable:
		return nil
	default:
		p = brokerParams(name)
	}
	b, _ := json.Marshal(p)
	return b
}

// A daemon with every service, each counting what reaches it.
type allServices struct {
	links *countingLinks
	br    *fakeBrokers
	cp    *fakeCheckpoints
	wr    *fakeWritable
}

func (a *allServices) reached() int {
	a.cp.mu.Lock()
	n := len(a.cp.envs)
	a.cp.mu.Unlock()
	a.wr.mu.Lock()
	n += a.wr.calls
	a.wr.mu.Unlock()
	return n + a.links.served() + len(a.br.served())
}

func startAll(t *testing.T) (string, *allServices) {
	t.Helper()
	a := &allServices{links: &countingLinks{}, br: &fakeBrokers{result: &daemonproto.RevokeResult{Revoked: "g1"}},
		cp: &fakeCheckpoints{result: &daemonproto.CheckpointsResult{Env: "alpha"}}, wr: &fakeWritable{result: &daemonproto.WritableResult{}}}
	dir := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t},
		Links: a.links, Brokers: a.br, Checkpoints: a.cp, Writable: a.wr})
	return dir, a
}

// The command line reaches every method of the table (stop and replace
// aside: they end the daemon), and nothing else is served.
func TestCLICallsTheTable(t *testing.T) {
	dir, a := startAll(t)
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	for _, m := range (&server{}).methods() {
		if m.name == daemonproto.MethodStop || m.name == daemonproto.MethodReplace {
			continue
		}
		var res json.RawMessage
		if err := c.Call(t.Context(), m.name, paramsFor(m.name), &res); err != nil {
			t.Errorf("%s: %v", m.name, err)
		}
	}
	before := a.reached()
	for _, name := range []string{daemonproto.MethodSubscribe, "frobnicate"} {
		if err := c.Call(t.Context(), name, nil, nil); daemonproto.Kind(err) != daemonproto.KindUnknownMethod {
			t.Errorf("%q: %v", name, err)
		}
	}
	if a.reached() != before {
		t.Error("an unknown method reached a service")
	}
}

// A socket hello of kind daemon is refused before any request, and
// nothing is served.
func TestSocketRefusesDaemonHellos(t *testing.T) {
	dir, a := startAll(t)
	c, err := daemonproto.Dial(t.Context(), Socket(dir), daemonproto.Options{Client: daemonproto.ClientDaemon, Version: "v9.9.9"})
	if err == nil {
		_ = c.Call(t.Context(), daemonproto.MethodAnswer, brokerParams(daemonproto.MethodAnswer), nil)
		c.Close()
		t.Error("a daemon hello was let through")
	}
	if daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("daemon: %v", err)
	}
	if n := a.reached(); n != 0 {
		t.Errorf("%d calls served", n)
	}
}

// A result the daemon cannot vouch for -- a control character, a bidi
// override -- never leaves, and nor does one over a frame.
func TestResultsAreChecked(t *testing.T) {
	dir, a := startAll(t)
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	params := json.RawMessage(`{"env":"alpha"}`)
	for _, bad := range []string{"\x1b[2J", "a\u202eb", "\x00"} {
		a.br.mu.Lock()
		a.br.result = &daemonproto.RequestsResult{Requests: []daemonproto.RequestView{{ID: 1, Kind: "egress", State: "pending", Effect: bad}}}
		a.br.mu.Unlock()
		var raw json.RawMessage
		if err := c.Call(t.Context(), daemonproto.MethodRequests, params, &raw); daemonproto.Kind(err) != daemonproto.KindInternal {
			t.Errorf("%q: %s %v", bad, raw, err)
		}
	}
	a.br.mu.Lock()
	a.br.result = bigResult{Big: strings.Repeat("x", daemonproto.MaxLine)}
	a.br.mu.Unlock()
	if err := c.Call(t.Context(), daemonproto.MethodGrants, params, nil); daemonproto.Kind(err) != daemonproto.KindInternal || !strings.Contains(err.Error(), "fit") {
		t.Errorf("oversize: %v", err)
	}
	if err := c.Ping(t.Context()); err != nil {
		t.Errorf("after an oversize result: %v", err)
	}
}

// Params are checked as a request's are: an object, or nothing; anything
// else reaches nothing.
func TestParamsAreChecked(t *testing.T) {
	dir, a := startAll(t)
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	for _, p := range []string{`[]`, `"alpha"`, `1`, `true`, ` [{"env":"alpha"}]`, `{"env":"../x"}`, `{"env":1}`, `null`, `{"env":"alpha","id":0}`} {
		if err := c.Call(t.Context(), daemonproto.MethodRequest, json.RawMessage(p), nil); daemonproto.Kind(err) != daemonproto.KindBadRequest {
			t.Errorf("%s: %v", p, err)
		}
	}
	// An always answer without the hash of the diff shown.
	if err := c.Call(t.Context(), daemonproto.MethodAnswer, json.RawMessage(`{"env":"alpha","id":1,"verdict":"allow","scope":"always"}`), nil); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("always without confirm_diff: %v", err)
	}
	if n := a.reached(); n != 0 {
		t.Errorf("%d bad calls served", n)
	}
	if err := c.Call(t.Context(), daemonproto.MethodPing, json.RawMessage(` {} `), nil); err != nil {
		t.Errorf("ping: %v", err)
	}
}

// bigResult is a result that checks out but is over a frame.
type bigResult struct{ Big string }

func (bigResult) Validate() error { return nil }
