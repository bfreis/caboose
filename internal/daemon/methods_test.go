package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// The allowlist: every method of the table and the kinds of client that
// may call it, written out. A method added to the table fails this test
// until it is listed here, which is the decision of whether the web page
// may call it; the table refuses any kind a method does not name.
func TestMethodTableAllowlist(t *testing.T) {
	cli, app, web := daemonproto.ClientCLI, daemonproto.ClientApp, daemonproto.ClientWeb
	want := map[string][]string{
		daemonproto.MethodPing:           {cli, app, web},
		daemonproto.MethodStatus:         {cli, app, web},
		daemonproto.MethodEnsureLink:     {cli, app},
		daemonproto.MethodRestartLink:    {cli, app},
		daemonproto.MethodStopLink:       {cli, app},
		daemonproto.MethodStop:           {cli, app},
		daemonproto.MethodReplace:        {cli, app},
		daemonproto.MethodRequests:       {cli, app, web},
		daemonproto.MethodRequest:        {cli, app, web},
		daemonproto.MethodAnswer:         {cli, app, web},
		daemonproto.MethodGrants:         {cli, app, web},
		daemonproto.MethodRevoke:         {cli, app, web},
		daemonproto.MethodPermissions:    {cli, app, web},
		daemonproto.MethodEditEgress:     {cli, app, web},
		daemonproto.MethodProposals:      {cli, app, web},
		daemonproto.MethodProposalDelete: {cli, app, web},
		daemonproto.MethodProposalApply:  {cli, app, web},
		daemonproto.MethodProposalBuild:  {cli, app, web},
		daemonproto.MethodCheckpoints:    {cli, app, web},
		daemonproto.MethodLinkLog:        {cli, app, web},
		daemonproto.MethodEnvDetails:     {cli, app, web},
		daemonproto.MethodDoctor:         {cli, app, web},
		daemonproto.MethodWritable:       {cli, app},
		// They end every session: never the page's.
		daemonproto.MethodSandboxStop:    {cli, app},
		daemonproto.MethodSandboxRestart: {cli, app},
		daemonproto.MethodWeb:            {cli, app},
		// The menu-bar app's alone: never the command line's, never the
		// page's.
		daemonproto.MethodAppAttach:     {app},
		daemonproto.MethodNotifications: {app},
		daemonproto.MethodDialogs:       {app},
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
		w, ok := want[m.name]
		if !ok {
			t.Errorf("%s is in the table but not in this allowlist: decide who may call it", m.name)
			continue
		}
		a, b := slices.Clone(m.clients), slices.Clone(w)
		slices.Sort(a)
		slices.Sort(b)
		if !slices.Equal(a, b) {
			t.Errorf("%s: callers %v, want %v", m.name, m.clients, w)
		}
		if m.newer != slices.Contains(newer, m.name) {
			t.Errorf("%s: served to a newer protocol is %v", m.name, m.newer)
		}
		if slices.Contains(m.clients, daemonproto.ClientDaemon) || slices.Contains(m.clients, "") {
			t.Errorf("%s names a kind no caller has", m.name)
		}
		if m.run == nil {
			t.Errorf("%s has no run", m.name)
		}
	}
	for name := range want {
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
	case daemonproto.MethodEnsureLink, daemonproto.MethodRestartLink, daemonproto.MethodStopLink, daemonproto.MethodCheckpoints,
		daemonproto.MethodEnvDetails, daemonproto.MethodDoctor:
		p = &daemonproto.EnvParams{Env: "alpha"}
	case daemonproto.MethodPing, daemonproto.MethodStatus, daemonproto.MethodStop, daemonproto.MethodWritable, daemonproto.MethodWeb:
		return nil
	case daemonproto.MethodAppAttach:
		p = &daemonproto.AppAttachParams{Capabilities: []string{daemonproto.CapNotifications}, Version: "v9.9.9"}
	case daemonproto.MethodNotifications:
		p = &daemonproto.NotificationsParams{}
	case daemonproto.MethodDialogs:
		p = &daemonproto.DialogsParams{}
	case daemonproto.MethodLinkLog:
		p = &daemonproto.LinkLogParams{Env: "alpha"}
	case daemonproto.MethodSandboxStop:
		p = &daemonproto.EnvParams{Env: "alpha"}
	case daemonproto.MethodSandboxRestart:
		p = &daemonproto.EnvParams{Env: "beta"} // a job of its own
	case daemonproto.MethodProposals:
		p = &daemonproto.EnvParams{Env: "alpha"}
	case daemonproto.MethodProposalDelete:
		p = &daemonproto.ProposalDeleteParams{Env: "alpha", File: "foo.toml", Hash: strings.Repeat("ab", 32)}
	case daemonproto.MethodProposalApply, daemonproto.MethodProposalBuild:
		p = &daemonproto.ProposalApplyParams{Env: "alpha", File: "foo.toml", Hash: strings.Repeat("ab", 32), Typed: "foo"}
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
	det   *fakeDetails
	pr    *fakeProposals
	jobs  *fakeJobs
}

func (a *allServices) reached() int {
	a.cp.mu.Lock()
	n := len(a.cp.envs)
	a.cp.mu.Unlock()
	a.wr.mu.Lock()
	n += a.wr.calls
	a.wr.mu.Unlock()
	a.det.mu.Lock()
	n += len(a.det.calls)
	a.det.mu.Unlock()
	n += a.pr.count()
	a.jobs.mu.Lock()
	n += len(a.jobs.running)
	a.jobs.mu.Unlock()
	return n + a.links.served() + len(a.br.served())
}

func startAll(t *testing.T) (string, *server, *allServices) {
	t.Helper()
	a := &allServices{links: &countingLinks{}, br: &fakeBrokers{result: &daemonproto.RevokeResult{Revoked: "g1"}},
		cp: &fakeCheckpoints{result: &daemonproto.CheckpointsResult{Env: "alpha"}}, wr: &fakeWritable{result: &daemonproto.WritableResult{}}, det: &fakeDetails{},
		pr: &fakeProposals{}, jobs: &fakeJobs{at: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}}
	dir, s := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t},
		Links: a.links, Brokers: a.br, Checkpoints: a.cp, Writable: a.wr, Details: a.det, Proposals: a.pr, Jobs: a.jobs})
	return dir, s, a
}

// The web page calls what its allowlist names, and nothing else reaches
// any service or does anything: stop does not stop, replace does not
// replace.
func TestWebCallsOnlyItsAllowlist(t *testing.T) {
	dir, s, a := startAll(t)
	for _, m := range s.methods() {
		before := a.reached()
		_, e := s.callWeb(t.Context(), m.name, paramsFor(m.name))
		if slices.Contains(m.clients, daemonproto.ClientWeb) {
			if e != nil {
				t.Errorf("%s from the web: %v", m.name, e)
			}
			continue
		}
		if webKind(e) != daemonproto.KindNotAllowed {
			t.Errorf("%s from the web: %v", m.name, e)
		}
		if a.reached() != before {
			t.Errorf("%s from the web reached a service", m.name)
		}
	}
	for _, name := range []string{daemonproto.MethodSubscribe, "web_token", "frobnicate", "Ping", ""} {
		if _, e := s.callWeb(t.Context(), name, nil); webKind(e) != daemonproto.KindUnknownMethod {
			t.Errorf("%q from the web: %v", name, e)
		}
	}
	// The daemon still runs, and was not asked to be replaced.
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	st, err := c.Status(t.Context())
	if err != nil || st.Daemon.Replacing {
		t.Fatalf("after the web's calls: %+v %v", st, err)
	}
	// The command line reaches every method it is listed for (stop last:
	// it stops), and no other.
	for _, m := range s.methods() {
		if m.name == daemonproto.MethodStop || m.name == daemonproto.MethodReplace {
			continue
		}
		var res json.RawMessage
		err := c.Call(t.Context(), m.name, paramsFor(m.name), &res)
		if !slices.Contains(m.clients, daemonproto.ClientCLI) {
			if daemonproto.Kind(err) != daemonproto.KindNotAllowed {
				t.Errorf("%s from the cli: %v", m.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s from the cli: %v", m.name, err)
		}
	}
	if err := c.Call(t.Context(), daemonproto.MethodSubscribe, nil, nil); daemonproto.Kind(err) != daemonproto.KindUnknownMethod {
		t.Errorf("subscribe: %v", err)
	}
}

// A socket hello of kind web or daemon is refused before any request,
// and nothing is served; the second check, after the handshake, refuses
// them too.
func TestSocketRefusesWebAndDaemonHellos(t *testing.T) {
	dir, _, a := startAll(t)
	for _, kind := range []string{daemonproto.ClientWeb, daemonproto.ClientDaemon} {
		c, err := daemonproto.Dial(t.Context(), Socket(dir), daemonproto.Options{Client: kind, Version: "v9.9.9"})
		if err == nil {
			_ = c.Call(t.Context(), daemonproto.MethodAnswer, brokerParams(daemonproto.MethodAnswer), nil)
			c.Close()
			t.Errorf("a %s hello was let through", kind)
		}
		if daemonproto.Kind(err) != daemonproto.KindBadRequest {
			t.Errorf("%s: %v", kind, err)
		}
	}
	if n := a.reached(); n != 0 {
		t.Errorf("%d calls served", n)
	}
	for _, kind := range []string{daemonproto.ClientWeb, daemonproto.ClientDaemon, "", "CLI"} {
		if _, ok := socketCaller(&daemonproto.Frame{Type: daemonproto.TypeHello, Client: kind}, daemonproto.CompatSame); ok {
			t.Errorf("socketCaller let %q through", kind)
		}
	}
	for _, kind := range []string{daemonproto.ClientCLI, daemonproto.ClientApp} {
		if c, ok := socketCaller(&daemonproto.Frame{Type: daemonproto.TypeHello, Client: kind}, daemonproto.CompatSame); !ok || c.Kind != kind {
			t.Errorf("socketCaller %q: %+v %v", kind, c, ok)
		}
	}
}

// One source of truth: the socket and the web page get the same answer
// from the same daemon state.
func TestSocketAndWebAgree(t *testing.T) {
	dir, s, a := startAll(t)
	now := time.Now().UTC().Truncate(time.Second)
	a.br.mu.Lock()
	a.br.result = &daemonproto.RequestsResult{Requests: []daemonproto.RequestView{{ID: 4, Kind: "egress", State: "pending", Created: now,
		Effect: "connect to api.example.com:443", Ask: &daemonproto.Ask{Context: "<b>the sandbox says</b>"}}}}
	a.br.mu.Unlock()
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	for _, tc := range []struct {
		method string
		params any
		into   func() any
	}{
		{daemonproto.MethodStatus, nil, func() any { return &daemonproto.StatusResult{} }},
		{daemonproto.MethodRequests, &daemonproto.RequestsParams{Env: "alpha"}, func() any { return &daemonproto.RequestsResult{} }},
	} {
		sock, web := tc.into(), tc.into()
		if err := c.Call(t.Context(), tc.method, tc.params, sock); err != nil {
			t.Fatalf("%s on the socket: %v", tc.method, err)
		}
		raw, _ := json.Marshal(tc.params)
		if tc.params == nil {
			raw = nil
		}
		b, e := s.callWeb(t.Context(), tc.method, raw)
		if e != nil {
			t.Fatalf("%s from the web: %v", tc.method, e)
		}
		if err := json.Unmarshal(b, web); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(sock, web) {
			t.Errorf("%s: socket %+v, web %+v", tc.method, sock, web)
		}
	}
}

// A result the daemon cannot vouch for -- a control character, a bidi
// override -- leaves by neither path, and nor does one over a frame.
func TestResultsAreCheckedOnBothPaths(t *testing.T) {
	dir, s, a := startAll(t)
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	params := json.RawMessage(`{"env":"alpha"}`)
	for _, bad := range []string{"\x1b[2J", "a\u202eb", "\x00"} {
		a.br.mu.Lock()
		a.br.result = &daemonproto.RequestsResult{Requests: []daemonproto.RequestView{{ID: 1, Kind: "egress", State: "pending", Effect: bad}}}
		a.br.mu.Unlock()
		if b, e := s.callWeb(t.Context(), daemonproto.MethodRequests, params); webKind(e) != daemonproto.KindInternal || b != nil {
			t.Errorf("web %q: %s %v", bad, b, e)
		}
		var raw json.RawMessage
		if err := c.Call(t.Context(), daemonproto.MethodRequests, params, &raw); daemonproto.Kind(err) != daemonproto.KindInternal {
			t.Errorf("socket %q: %s %v", bad, raw, err)
		}
	}
	a.br.mu.Lock()
	a.br.result = bigResult{Big: strings.Repeat("x", daemonproto.MaxLine)}
	a.br.mu.Unlock()
	if _, e := s.callWeb(t.Context(), daemonproto.MethodGrants, params); webKind(e) != daemonproto.KindInternal || !strings.Contains(e.Message, "fit") {
		t.Errorf("oversize from the web: %v", e)
	}
	// A result the daemon has no check for never reaches the page.
	a.br.mu.Lock()
	a.br.result = map[string]string{"x": "y"}
	a.br.mu.Unlock()
	if _, e := s.callWeb(t.Context(), daemonproto.MethodGrants, params); webKind(e) != daemonproto.KindInternal {
		t.Errorf("an unchecked result from the web: %v", e)
	}
	a.br.mu.Lock()
	a.br.result = (*daemonproto.RevokeResult)(nil)
	a.br.mu.Unlock()
	if b, e := s.callWeb(t.Context(), daemonproto.MethodGrants, params); e != nil || string(b) != "{}" {
		t.Errorf("a nil result: %s %v", b, e)
	}
}

// Params from the web page are checked as a request's are: an object, or
// nothing; anything else reaches nothing.
func TestWebParamsAreChecked(t *testing.T) {
	_, s, a := startAll(t)
	for _, p := range []string{`[]`, `"alpha"`, `1`, `true`, ` [{"env":"alpha"}]`, `{"env":"../x"}`, `{"env":1}`, `null`, `{"env":"alpha","id":0}`} {
		if _, e := s.callWeb(t.Context(), daemonproto.MethodRequest, json.RawMessage(p)); webKind(e) != daemonproto.KindBadRequest {
			t.Errorf("%s: %v", p, e)
		}
	}
	// An always answer without the hash of the diff shown.
	if _, e := s.callWeb(t.Context(), daemonproto.MethodAnswer, json.RawMessage(`{"env":"alpha","id":1,"verdict":"allow","scope":"always"}`)); webKind(e) != daemonproto.KindBadRequest {
		t.Errorf("always without confirm_diff: %v", e)
	}
	if n := a.reached(); n != 0 {
		t.Errorf("%d bad calls served", n)
	}
	if _, e := s.callWeb(t.Context(), daemonproto.MethodPing, json.RawMessage(` {} `)); e != nil {
		t.Errorf("ping: %v", e)
	}
}

// bigResult is a result that checks out but is over a frame.
type bigResult struct{ Big string }

func (bigResult) Validate() error { return nil }

// webKind is the kind of callWeb's error, "" for none.
func webKind(e *daemonproto.Error) string {
	if e == nil {
		return ""
	}
	return e.Kind
}
