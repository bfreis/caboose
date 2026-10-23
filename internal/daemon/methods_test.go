package daemon

import (
	"context"
	"encoding/json"
	"fmt"
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
	envs  []daemonproto.EnvStatus // what Envs says
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
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.envs == nil {
		return []daemonproto.EnvStatus{{Name: "alpha"}}
	}
	return l.envs
}

func (l *countingLinks) setEnvs(envs []daemonproto.EnvStatus) {
	l.mu.Lock()
	l.envs = envs
	l.mu.Unlock()
}

// paramsFor are valid params for each method of the table.
func paramsFor(name string) json.RawMessage {
	var p any
	switch name {
	case daemonproto.MethodReplace:
		p = &daemonproto.ReplaceParams{Version: "v9.9.9"}
	case daemonproto.MethodEnsureLink, daemonproto.MethodRestartLink, daemonproto.MethodStopLink:
		p = &daemonproto.EnvParams{Env: "alpha"}
	default:
		return nil
	}
	b, _ := json.Marshal(p)
	return b
}

// A daemon with every service, each counting what reaches it.
type allServices struct {
	links *countingLinks
}

func (a *allServices) reached() int {
	return a.links.served()
}

func startAll(t *testing.T) (string, *allServices) {
	t.Helper()
	a := &allServices{links: &countingLinks{}}
	dir := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t},
		Links: a.links})
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
		_ = c.Call(t.Context(), daemonproto.MethodEnsureLink, paramsFor(daemonproto.MethodEnsureLink), nil)
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
	for _, bad := range []string{"\x1b[2J", "a\u202eb", "\x00"} {
		a.links.setEnvs([]daemonproto.EnvStatus{{Name: "alpha", LastError: bad}})
		var raw json.RawMessage
		if err := c.Call(t.Context(), daemonproto.MethodStatus, nil, &raw); daemonproto.Kind(err) != daemonproto.KindInternal {
			t.Errorf("%q: %s %v", bad, raw, err)
		}
	}
	// Each environment's error within its bound, all of them over a frame.
	var many []daemonproto.EnvStatus
	for i := range daemonproto.MaxLine/4000 + 1 {
		many = append(many, daemonproto.EnvStatus{Name: fmt.Sprintf("e%d", i), LastError: strings.Repeat("x", 4000)})
	}
	a.links.setEnvs(many)
	if err := c.Call(t.Context(), daemonproto.MethodStatus, nil, nil); daemonproto.Kind(err) != daemonproto.KindInternal || !strings.Contains(err.Error(), "fit") {
		t.Errorf("oversize: %v", err)
	}
	a.links.setEnvs(nil)
	if err := c.Ping(t.Context()); err != nil {
		t.Errorf("after an oversize result: %v", err)
	}
}

// Params are checked as a request's are: an object, or nothing; anything
// else reaches nothing.
func TestParamsAreChecked(t *testing.T) {
	dir, a := startAll(t)
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	for _, p := range []string{`[]`, `"alpha"`, `1`, `true`, ` [{"env":"alpha"}]`, `{"env":"../x"}`, `{"env":1}`, `null`} {
		if err := c.Call(t.Context(), daemonproto.MethodEnsureLink, json.RawMessage(p), nil); daemonproto.Kind(err) != daemonproto.KindBadRequest {
			t.Errorf("%s: %v", p, err)
		}
	}
	if n := a.reached(); n != 0 {
		t.Errorf("%d bad calls served", n)
	}
	if err := c.Call(t.Context(), daemonproto.MethodPing, json.RawMessage(` {} `), nil); err != nil {
		t.Errorf("ping: %v", err)
	}
}

// startServer runs a daemon in this process with opts (in a directory of
// its own) and returns the directory.
func startServer(t *testing.T, opts Options) string {
	t.Helper()
	h := newHarness(t)
	opts.Dir = h.dir
	runInProcess(t, opts)
	return h.dir
}

func dialAs(t *testing.T, dir, kind string, protocol int) *daemonproto.Client {
	t.Helper()
	c, err := daemonproto.Dial(context.Background(), Socket(dir), daemonproto.Options{Client: kind, Version: "v9.9.9", Protocol: protocol})
	if err != nil {
		t.Fatalf("dial as %s: %v", kind, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
