package launcher

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/daemonproto"
)

// brokerFake is what a fakeDaemon answers the broker's calls with.
type brokerFake struct {
	mu       sync.Mutex
	List     *daemonproto.RequestsResult
	One      *daemonproto.RequestResult
	Gs       *daemonproto.GrantsResult
	Perm     *daemonproto.PermissionsResult
	AnswerFn func(daemonproto.AnswerParams) (*daemonproto.AnswerResult, error)
	Err      error // the error of every broker call

	answers []daemonproto.AnswerParams
	listed  []bool // each listing's All
	revoked []string
}

func (f *fakeDaemon) Requests(_ context.Context, _ string, all bool, _ int) (*daemonproto.RequestsResult, error) {
	f.br.mu.Lock()
	defer f.br.mu.Unlock()
	f.br.listed = append(f.br.listed, all)
	if f.br.Err != nil {
		return nil, f.br.Err
	}
	return f.br.List, nil
}

func (f *fakeDaemon) Request(_ context.Context, _ string, id uint64) (*daemonproto.RequestResult, error) {
	if f.br.Err != nil {
		return nil, f.br.Err
	}
	return f.br.One, nil
}

func (f *fakeDaemon) Answer(_ context.Context, p daemonproto.AnswerParams) (*daemonproto.AnswerResult, error) {
	f.br.mu.Lock()
	f.br.answers = append(f.br.answers, p)
	fn, err := f.br.AnswerFn, f.br.Err
	f.br.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return fn(p)
}

func (f *fakeDaemon) Grants(context.Context, string) (*daemonproto.GrantsResult, error) {
	if f.br.Err != nil {
		return nil, f.br.Err
	}
	return f.br.Gs, nil
}

func (f *fakeDaemon) Revoke(_ context.Context, _, grant string) (*daemonproto.RevokeResult, error) {
	f.br.mu.Lock()
	f.br.revoked = append(f.br.revoked, grant)
	f.br.mu.Unlock()
	if f.br.Err != nil {
		return nil, f.br.Err
	}
	return &daemonproto.RevokeResult{Revoked: grant}, nil
}

func (f *fakeDaemon) Permissions(context.Context, string) (*daemonproto.PermissionsResult, error) {
	if f.br.Err != nil {
		return nil, f.br.Err
	}
	return f.br.Perm, nil
}

func sampleView(id uint64, kind, effect string) daemonproto.RequestView {
	return daemonproto.RequestView{ID: id, Kind: kind, Effect: effect, State: "pending", Created: time.Now().Add(-3 * time.Minute),
		Ask: &daemonproto.Ask{Context: "I need the docs", Options: []string{"once", "an hour"}, Recommendation: "once", Meanwhile: "reading offline"}}
}

func TestRequestsListShowsEffectThenSandboxText(t *testing.T) {
	fd := &fakeDaemon{}
	v := sampleView(41, "egress", "Allowing lets the sandbox connect to api.example.com:443")
	v.Ask.Context = "evil \x1b[31mred\u202e text"
	fd.br.List = &daemonproto.RequestsResult{Requests: []daemonproto.RequestView{v}}
	a, out, _ := newCLIApp(t, fd)
	if err := a.Requests(nil); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	i, j := strings.Index(s, "Allowing lets the sandbox"), strings.Index(s, "the sandbox says:")
	if !strings.Contains(s, "#41") || !strings.Contains(s, "3m ago") || !strings.Contains(s, "egress") || i < 0 || j < i {
		t.Errorf("the listing:\n%s", s)
	}
	if strings.ContainsAny(s, "\x1b\u202e") || !strings.Contains(s, `\u001B`) {
		t.Errorf("the sandbox's text reached the terminal raw:\n%q", s)
	}
	if !strings.Contains(s, "requests approve N") || fd.br.listed[0] {
		t.Errorf("hint or pending-only: %q %v", s, fd.br.listed)
	}
	if !fd.dials[0] {
		t.Error("the broker commands start the daemon")
	}
}

func TestRequestsListEmptyAndAll(t *testing.T) {
	fd := &fakeDaemon{}
	fd.br.List = &daemonproto.RequestsResult{}
	a, out, _ := newCLIApp(t, fd)
	if err := a.Requests([]string{"--all"}); err != nil || !fd.br.listed[0] || !strings.Contains(out.String(), "no requests in environment default") {
		t.Errorf("err %v, %q", err, out)
	}
}

func TestRequestsShow(t *testing.T) {
	fd := &fakeDaemon{}
	v := sampleView(7, "open_url", "Opens https://example.com")
	v.Key, v.Session, v.Attempts = "https://example.com", "s1", 3
	fd.br.One = &daemonproto.RequestResult{Request: v}
	a, out, _ := newCLIApp(t, fd)
	if err := a.Requests([]string{"show", "#7"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"asked 3 times", "key: https://example.com", "session (as the sandbox names it): s1", "meanwhile: reading offline"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
}

func allowed(id uint64, scope string, hours int) *daemonproto.AnswerResult {
	return &daemonproto.AnswerResult{Request: daemonproto.RequestView{ID: id, Kind: "egress", State: "allowed", Scope: scope, Hours: hours},
		Effect: "the sandbox may connect", Recorded: true, Grant: "g12", Also: []uint64{8, 9}, OpenError: "no browser"}
}

func TestApproveScopes(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		scope string
		hours int
	}{
		{[]string{"approve", "5"}, "once", 0},
		{[]string{"approve", "5", "--for", "8h"}, "hours", 8},
		{[]string{"approve", "5", "--for=2d"}, "hours", 48},
		{[]string{"approve", "#5", "--for", "168h"}, "hours", 168},
	} {
		fd := &fakeDaemon{}
		fd.br.AnswerFn = func(p daemonproto.AnswerParams) (*daemonproto.AnswerResult, error) {
			return allowed(p.ID, p.Scope, p.Hours), nil
		}
		a, out, _ := newCLIApp(t, fd)
		if err := a.Requests(tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		got := fd.br.answers[0]
		if got.ID != 5 || got.Verdict != "allow" || got.Scope != tc.scope || got.Hours != tc.hours || got.ConfirmDiff != "" || got.Env != "default" {
			t.Errorf("%v sent %+v", tc.args, got)
		}
		for _, want := range []string{"Request #5 allowed", "the sandbox may connect", "grant g12", "#8, #9", "the URL did not open: no browser"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%v: output lacks %q:\n%s", tc.args, want, out)
			}
		}
	}
}

func TestApproveRefusesBadDurations(t *testing.T) {
	for _, d := range []string{"0h", "169h", "8d", "90m", "8", "-1h", "1.5h", "h", ""} {
		fd := &fakeDaemon{}
		a, _, _ := newCLIApp(t, fd)
		if err := a.Requests([]string{"approve", "1", "--for", d}); err == nil {
			t.Errorf("--for %q accepted", d)
		}
		if len(fd.dials) != 0 {
			t.Errorf("--for %q reached the daemon", d)
		}
	}
	for _, args := range [][]string{{"approve", "1", "--for", "2h", "--always"}, {"approve", "1", "--always", "--for", "2h"}, {"approve"}, {"approve", "x"}, {"approve", "0"},
		{"approve", "1", "--for"}, {"approve", "1", "--bogus"}, {"deny"}, {"deny", "1", "2"}, {"show"}, {"frob"}, {"--all", "x"}} {
		fd := &fakeDaemon{}
		a, _, _ := newCLIApp(t, fd)
		if err := a.Requests(args); err == nil || len(fd.dials) != 0 {
			t.Errorf("%v: err %v, dials %v", args, err, fd.dials)
		}
	}
}

func TestDeny(t *testing.T) {
	fd := &fakeDaemon{}
	fd.br.AnswerFn = func(p daemonproto.AnswerParams) (*daemonproto.AnswerResult, error) {
		return &daemonproto.AnswerResult{Request: daemonproto.RequestView{ID: p.ID, State: "denied"}, Recorded: true}, nil
	}
	a, out, _ := newCLIApp(t, fd)
	if err := a.Requests([]string{"deny", "3"}); err != nil {
		t.Fatal(err)
	}
	if g := fd.br.answers[0]; g.Verdict != "deny" || g.Scope != "" || !strings.Contains(out.String(), "Request #3 denied.") {
		t.Errorf("%+v %q", g, out)
	}
}

// alwaysFake offers request 5 always with diff, and records an answer
// only when confirmed with its hash.
func alwaysFake(fd *fakeDaemon, diff []string, hash string) {
	fd.br.One = &daemonproto.RequestResult{Request: daemonproto.RequestView{ID: 5, State: "pending"},
		Offers: []daemonproto.Offer{{Scope: "once", Effect: "allows that once"}, {Scope: "always", Effect: "egress_allow gains api.example.com", Diff: diff, Hash: hash}}}
	fd.br.AnswerFn = func(p daemonproto.AnswerParams) (*daemonproto.AnswerResult, error) {
		if p.ConfirmDiff != hash {
			return nil, daemonproto.NewError(daemonproto.KindStaleDiff, "stale")
		}
		return &daemonproto.AnswerResult{Request: daemonproto.RequestView{ID: p.ID, State: "allowed", Scope: "always"}, Effect: "egress_allow gains api.example.com",
			DiffHash: hash, Recorded: true}, nil
	}
}

const testHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestApproveAlwaysShowsTheDiffAndNeedsATypedYes(t *testing.T) {
	diff := []string{" [vm.default]", "-egress_allow = []", "+egress_allow = [\"api.example.com\"]", "+# \x1b[31mevil"}
	fd := &fakeDaemon{}
	alwaysFake(fd, diff, testHash)
	a, out, errb := newCLIApp(t, fd)
	a.Terminal = scriptedTerminal("yes\n")
	if err := a.Requests([]string{"approve", "5", "--always"}); err != nil {
		t.Fatal(err)
	}
	if len(fd.br.answers) != 1 || fd.br.answers[0].ConfirmDiff != testHash || fd.br.answers[0].Scope != "always" {
		t.Fatalf("calls %+v", fd.br.answers)
	}
	s := out.String()
	if !strings.Contains(s, `+egress_allow = ["api.example.com"]`) || !strings.Contains(s, "egress_allow gains") || strings.Contains(s, "\x1b") || !strings.Contains(s, "allowed (always)") {
		t.Errorf("output:\n%s", s)
	}
	if !strings.Contains(errb.String(), "Type yes") {
		t.Errorf("no question: %q", errb)
	}
}

func TestApproveAlwaysAnythingButYesWritesNothing(t *testing.T) {
	for _, ans := range []string{"y\n", "no\n", "\n", "YES\n", ""} {
		fd := &fakeDaemon{}
		alwaysFake(fd, []string{"+x"}, testHash)
		a, _, _ := newCLIApp(t, fd)
		a.Terminal = scriptedTerminal(ans)
		if err := a.Requests([]string{"approve", "5", "--always"}); err == nil || !strings.Contains(err.Error(), "nothing was written") {
			t.Errorf("answer %q: %v", ans, err)
		}
		if len(fd.br.answers) != 0 {
			t.Errorf("answer %q: calls %+v", ans, fd.br.answers)
		}
	}
}

// A request not offered always says why, and answers nothing.
func TestApproveAlwaysNotOffered(t *testing.T) {
	fd := &fakeDaemon{}
	alwaysFake(fd, nil, testHash)
	fd.br.One.Offers = fd.br.One.Offers[:1]
	fd.br.One.Withheld = []daemonproto.Withheld{{Scope: "always", Why: "trust = hostile asks before opening any URL"}}
	a, _, _ := newCLIApp(t, fd)
	a.Terminal = scriptedTerminal("yes\n")
	if err := a.Requests([]string{"approve", "5", "--always"}); err == nil || !strings.Contains(err.Error(), "hostile") || !strings.Contains(err.Error(), "Nothing was changed") {
		t.Errorf("%v", err)
	}
	fd.br.One = &daemonproto.RequestResult{Request: daemonproto.RequestView{ID: 5, State: "denied"}}
	if err := a.Requests([]string{"approve", "5", "--always"}); err == nil || !strings.Contains(err.Error(), "denied, not pending") {
		t.Errorf("%v", err)
	}
	// A daemon from before offers sends none for a pending request.
	fd.br.One = &daemonproto.RequestResult{Request: daemonproto.RequestView{ID: 5, State: "pending"}}
	if err := a.Requests([]string{"approve", "5", "--always"}); err == nil || !strings.Contains(err.Error(), "caboose daemon restart") {
		t.Errorf("%v", err)
	}
	if len(fd.br.answers) != 0 {
		t.Errorf("calls %+v", fd.br.answers)
	}
}

func TestApproveAlwaysWithoutATerminalAsksNothing(t *testing.T) {
	fd := &fakeDaemon{}
	alwaysFake(fd, []string{"+x"}, testHash)
	a, _, _ := newCLIApp(t, fd)
	a.Terminal = func() (io.ReadCloser, error) { return nil, errors.New("stdin is not a terminal") }
	err := a.Requests([]string{"approve", "5", "--always"})
	if err == nil || !strings.Contains(err.Error(), "no terminal") || len(fd.dials) != 0 {
		t.Errorf("err %v, dials %v", err, fd.dials)
	}
}

func TestBrokerErrorsAreFriendly(t *testing.T) {
	for kind, want := range map[string]string{
		daemonproto.KindUnknownMethod:     "caboose daemon restart",
		daemonproto.KindNotOwner:          "no link serves",
		daemonproto.KindNotFound:          "requests --all",
		daemonproto.KindAlreadyAnswered:   "first answer stands",
		daemonproto.KindStaleDiff:         "changed since",
		daemonproto.KindNotAllowed:        "may not",
		daemonproto.KindBrokerUnavailable: "cannot serve",
	} {
		fd := &fakeDaemon{}
		fd.br.Err = &daemonproto.Error{Kind: kind, Message: map[string]string{
			daemonproto.KindNotOwner: "no link serves default", daemonproto.KindNotAllowed: "a web client may not answer",
			daemonproto.KindBrokerUnavailable: "the broker cannot serve now"}[kind]}
		a, _, _ := newCLIApp(t, fd)
		err := a.Requests([]string{"deny", "1"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", kind, err, want)
		}
	}
}

func TestGrantsAndRevoke(t *testing.T) {
	fd := &fakeDaemon{}
	fd.br.Gs = &daemonproto.GrantsResult{Grants: []daemonproto.GrantView{
		{ID: "g1", Kind: "egress", Key: "egress:api.example.com", Expires: time.Now().Add(3 * time.Hour), LeftSeconds: 3 * 3600, Request: 4, Addrs: []string{"10.0.0.5"}},
		{ID: "g2", Kind: "alias", Key: "alias:x", Once: true, LeftSeconds: 600}}}
	a, out, _ := newCLIApp(t, fd)
	if err := a.Grants(nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"g1", "egress:api.example.com", "3h00m left", "request #4", "10.0.0.5", "once, within 10m", "grants revoke ID"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("grants lacks %q:\n%s", want, out)
		}
	}
	out.Reset()
	if err := a.Grants([]string{"revoke", "g1"}); err != nil || fd.br.revoked[0] != "g1" || !strings.Contains(out.String(), "Grant g1 ended") {
		t.Errorf("revoke: %v %v %q", err, fd.br.revoked, out)
	}
	for _, bad := range [][]string{{"revoke"}, {"x"}, {"revoke", "a", "b"}} {
		if err := a.Grants(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestPermissionsOutput(t *testing.T) {
	fd := &fakeDaemon{}
	fd.br.Perm = &daemonproto.PermissionsResult{Env: "default", Trust: "hostile", Isolation: "vm", Profile: "vm.default",
		Roots:    []daemonproto.RootPerm{{Name: "dev", Host: "/h/dev", Container: "/work/dev", Writable: []string{"p"}}, {Host: "/h/ro", Container: "/work/ro", ReadOnly: true}},
		Aliases:  []daemonproto.AliasPerm{{Name: "deploy", Command: []string{"/usr/bin/true", "x"}, Policy: "ask"}},
		HostExec: daemonproto.HostExecPerm{Configured: true, Why: "trust = hostile: the link does not offer host exec"},
		OpenURLs: daemonproto.OpenURLsPerm{Mode: "ask", Allow: []string{"https://example.com"}},
		Egress:   daemonproto.EgressPerm{On: true, Ports: "22 80 443", Allow: []string{"a.example"}, Default: "asks", Network: "through this machine"},
		Grants:   []daemonproto.GrantView{{ID: "g3", Kind: "egress", Key: "k", LeftSeconds: 60}},
		Mods:     &daemonproto.ModsPerm{Sessions: 2, WithMod: 1},
		Drift:    []string{"the VM was created with trust fallible; the configuration says hostile", "the VM mounts /h/old at /work/old"}}
	a, out, _ := newCLIApp(t, fd)
	if err := a.Permissions(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"drift       : the VM was created with trust fallible; the configuration says hostile\n              the VM mounts /h/old",
		"takes effect at 'caboose -e default restart'", "hostile", "vm (profile vm.default)", "/h/dev -> /work/dev: read-only, except p", "/h/ro -> /work/ro: read-only", "deploy: /usr/bin/true x", "policy ask",
		"host exec   : off (trust = hostile", "https://example.com", "a.example", "g3", "2 Claude Code sessions, 1 without caboose's mod"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("permissions lacks %q:\n%s", want, out)
		}
	}
	// No drift, no note.
	fd.br.Perm.Drift = nil
	out.Reset()
	if err := a.Permissions(); err != nil || strings.Contains(out.String(), "drift") || strings.Contains(out.String(), "restart") {
		t.Errorf("without drift: %v\n%s", err, out)
	}
}

func TestBrokerCommandsWhenTheDaemonIsUnreachable(t *testing.T) {
	fd := &fakeDaemon{Dial: errors.New("nothing answers")}
	a, _, _ := newCLIApp(t, fd)
	err := a.Requests(nil)
	if err == nil || !strings.Contains(err.Error(), "caboose daemon restart") {
		t.Errorf("%v", err)
	}
}

func scriptedTerminal(answers string) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(answers)), nil }
}

// status says how many requests wait, right after the link line and before
// the early return, from the status it already asked for; the daemon is
// never started for it.
func TestStatusSaysPendingRequests(t *testing.T) {
	for _, tc := range []struct {
		pending int
		want    string
	}{{3, "requests  : 3 pending\n\nnot running"}, {0, "\nnot running"}} {
		fd := &fakeDaemon{Stat: &daemonproto.StatusResult{
			Daemon: daemonproto.DaemonInfo{PID: 1, Version: "v1", Protocol: 1, Started: time.Now()},
			Envs:   []daemonproto.EnvStatus{{Name: "default", Sandbox: daemonproto.SandboxRunning, Link: daemonproto.LinkRunning, Pending: tc.pending}}}}
		a, out, _ := newCLIApp(t, fd)
		a.Backend = &backendtest.Fake{}
		a.Cfg.Image, a.Cfg.Container, a.Cfg.KeepVersions = "img", "box", 2
		if err := a.Status(); err != nil {
			t.Fatal(err)
		}
		s := out.String()
		i := strings.Index(s, "link      :")
		j := strings.Index(s[i:], "\n")
		if !strings.HasPrefix(s[i+j+1:], tc.want) && !strings.HasPrefix(s[i+j:], tc.want) {
			t.Errorf("pending %d: after the link line:\n%s", tc.pending, s[i:])
		}
		if strings.Contains(s, "requests  :") != (tc.pending > 0) {
			t.Errorf("pending %d: requests line present = %v", tc.pending, !(tc.pending > 0))
		}
		if len(fd.dials) != 1 || fd.dials[0] {
			t.Errorf("dials (start) %v", fd.dials)
		}
	}
}
