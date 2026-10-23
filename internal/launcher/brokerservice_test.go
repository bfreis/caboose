package launcher

import (
	"encoding/json"
	"errors"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/policy"
)

// A config.toml with comments, a list already in the way of the edit, an
// alias and the vm profile an egress answer writes to.
const brokerConfig = `# my caboose config -- hand edited
isolation = "vm.default"   # the VM, for the VPN

[vm.default]
# the outbound proxy
egress = true
egress_allow = ["*.corp.example"]
cpus = 4   # a comment on a line the edit leaves alone

[link]
open_urls_allow = ["https://docs.example.com"]   # docs I read

# a host command
[alias.deploy]
description = "deploy the thing"
command = ["/usr/bin/true", "now"]
policy = "ask"
`

type fakeOpener struct {
	mu     sync.Mutex
	opened []string
	err    error
}

func (o *fakeOpener) OpenURL(u string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opened = append(o.opened, u)
	return o.err
}

// brokerWorld is an environment, alpha, with a broker opened the way a
// link opens it (a real link.lock, the real config writer and settings),
// and DaemonLinks serving it.
type brokerWorld struct {
	t      *testing.T
	home   string
	envDir string
	cfg    string // config.toml's path
	opener *fakeOpener
	b      *broker.Broker
	l      *DaemonLinks
	mods   func() (agentproto.ModsInfo, bool)
	unlock func() // lets go of link.lock
	// box is every app's sandbox: absent unless a test makes it.
	box *backendtest.Fake
}

func newBrokerWorld(t *testing.T, body string) *brokerWorld {
	t.Helper()
	home := t.TempDir()
	vars := map[string]string{"HOME": home, "CABOOSE_HOME": home}
	getenv := func(k string) string { return vars[k] }
	w := &brokerWorld{t: t, home: home, envDir: config.EnvDirFor(home, "alpha"), opener: &fakeOpener{}, box: &backendtest.Fake{}}
	w.cfg = filepath.Join(w.envDir, config.FileName)
	if err := os.MkdirAll(filepath.Join(w.envDir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	b, unlock, err := openTestBroker(home, "alpha", w.opener)
	if err != nil {
		t.Fatal(err)
	}
	w.b, w.unlock = b, unlock
	t.Cleanup(unlock)
	t.Cleanup(func() { w.b.Close() })
	w.l = NewDaemonLinks(DaemonLinksOptions{Dir: filepath.Join(home, "daemon"), CabooseHome: home,
		NewApp: func(env string) (*App, error) {
			app, err := DaemonApp(getenv, "")(env)
			if err == nil {
				app.Backend = w.box
			}
			return app, err
		}})
	t.Cleanup(w.l.Close)
	w.l.brokerHook = func(env string) *brokerHandle {
		if env != "alpha" {
			return nil
		}
		return &brokerHandle{b: w.b, mods: func() (agentproto.ModsInfo, bool) {
			if w.mods == nil {
				return agentproto.ModsInfo{}, false
			}
			return w.mods()
		}}
	}
	return w
}

// openTestBroker opens env's broker the way a link does: under its real
// link.lock (taken, and let go of by the returned func), with the real
// settings and config writer, on the config.toml of the environment under
// home. Nothing is writable, as in a link today.
func openTestBroker(home, env string, opener broker.Opener) (*broker.Broker, func(), error) {
	vars := map[string]string{"HOME": home, "CABOOSE_HOME": home}
	getenv := func(k string) string { return vars[k] }
	envDir := config.EnvDirFor(home, env)
	lock, unlock, err := lockLinkFile(filepath.Join(envDir, "data"))
	if err != nil {
		return nil, nil, err
	}
	b, err := broker.Open(broker.Options{Home: home, Env: env, Owner: broker.LockOwner(lock),
		Source: func() (broker.Settings, error) {
			c, err := config.Load(getenv, config.OSFS{}, env)
			if err != nil {
				return broker.Settings{}, err
			}
			return brokerSettings(c, envDir), nil
		},
		Writable:     func() ([]string, error) { return []string{"/"}, nil },
		ConfigWriter: &configWriter{envDir: envDir}, Opener: opener})
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return b, unlock, nil
}

// call is a request to the daemon's brokers as a client of kind who.
func (w *brokerWorld) call(who, method string, params any) (any, error) {
	w.t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		w.t.Fatal(err)
	}
	return w.l.Call(w.t.Context(), who, method, "alpha", raw)
}

func (w *brokerWorld) mustCall(method string, params any) any {
	w.t.Helper()
	res, err := w.call(daemonproto.ClientCLI, method, params)
	if err != nil {
		w.t.Fatalf("%s: %v", method, err)
	}
	return res
}

func (w *brokerWorld) answer(id uint64, p daemonproto.AnswerParams) (*daemonproto.AnswerResult, error) {
	w.t.Helper()
	p.Env, p.ID = "alpha", id
	res, err := w.call(daemonproto.ClientCLI, daemonproto.MethodAnswer, p)
	if err != nil {
		return nil, err
	}
	return res.(*daemonproto.AnswerResult), nil
}

func egressReq(host string, addrs ...string) policy.Request {
	r := policy.Request{Kind: policy.KindEgress, Host: host, Port: 443}
	for _, a := range addrs {
		r.Addrs = append(r.Addrs, netip.MustParseAddr(a))
	}
	return r
}

func urlReq(u, origin string) policy.Request {
	return policy.Request{Kind: policy.KindOpenURL, URL: u, Origin: origin}
}

// file files an ask and returns its number.
func (w *brokerWorld) file(a broker.Ask) uint64 {
	w.t.Helper()
	o, err := w.b.File(a)
	if err != nil || o.Verdict != broker.Pending {
		w.t.Fatalf("filed %+v: %+v, %v", a.Request, o, err)
	}
	return o.Request
}

func (w *brokerWorld) decide(r policy.Request) string {
	w.t.Helper()
	o, err := w.b.Decide(r)
	if err != nil {
		w.t.Fatal(err)
	}
	return o.Verdict
}

func (w *brokerWorld) config() string {
	w.t.Helper()
	b, err := os.ReadFile(w.cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	return string(b)
}

func kindOf(err error) string { return daemonproto.Kind(err) }

func TestBrokerCallsNeedTheLinkThatOwnsTheBroker(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	// No hook: the daemon's own links, none of which runs alpha's.
	w.l.brokerHook = nil
	for _, m := range []string{daemonproto.MethodRequests, daemonproto.MethodGrants, daemonproto.MethodPermissions} {
		_, err := w.call(daemonproto.ClientCLI, m, &daemonproto.EnvParams{Env: "alpha"})
		if kindOf(err) != daemonproto.KindNotOwner || !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "link") {
			t.Errorf("%s: %v", m, err)
		}
	}
	// Another environment the hook knows nothing of.
	w.l.brokerHook = func(string) *brokerHandle { return nil }
	if _, err := w.l.Call(t.Context(), daemonproto.ClientCLI, daemonproto.MethodGrants, "beta", []byte(`{"env":"beta"}`)); kindOf(err) != daemonproto.KindNotOwner {
		t.Errorf("beta: %v", err)
	}
	if _, err := w.l.Call(t.Context(), daemonproto.ClientCLI, daemonproto.MethodGrants, "../x", []byte(`{}`)); kindOf(err) != daemonproto.KindBadRequest {
		t.Errorf("bad env: %v", err)
	}
}

// A broker whose link let go of link.lock -- and one whose lock another
// daemon took meanwhile -- answers not_owner to everything, and decides
// nothing.
func TestBrokerCallsAfterTheLinkLetGo(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	id := w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5")})
	w.unlock()
	other, unlockOther, err := lockLinkFile(filepath.Join(w.envDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlockOther()
	_ = other
	for _, m := range []string{daemonproto.MethodRequests, daemonproto.MethodGrants, daemonproto.MethodPermissions} {
		if _, err := w.call(daemonproto.ClientCLI, m, &daemonproto.EnvParams{Env: "alpha"}); kindOf(err) != daemonproto.KindNotOwner {
			t.Errorf("%s: %v", m, err)
		}
	}
	if _, err := w.answer(id, daemonproto.AnswerParams{Verdict: "allow", Scope: "hours", Hours: 1}); kindOf(err) != daemonproto.KindNotOwner {
		t.Errorf("answer: %v", err)
	}
	if _, err := w.call(daemonproto.ClientCLI, daemonproto.MethodRevoke, &daemonproto.RevokeParams{Env: "alpha", Grant: "g1"}); kindOf(err) != daemonproto.KindNotOwner {
		t.Errorf("revoke: %v", err)
	}
}

func TestBrokerListsAndShowsRequests(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	id := w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5"), Context: "need the API", Options: []string{"once", "an hour"},
		Recommendation: "once", Meanwhile: "waiting", Session: "s1"})
	w.decide(urlReq("https://example.com/a", "https://example.com"))
	res := w.mustCall(daemonproto.MethodRequests, &daemonproto.RequestsParams{Env: "alpha"}).(*daemonproto.RequestsResult)
	if len(res.Requests) != 2 || res.Truncated {
		t.Fatalf("%+v", res)
	}
	var egress daemonproto.RequestView
	for _, v := range res.Requests {
		if v.ID == id {
			egress = v
		}
	}
	if egress.Kind != "egress" || egress.State != "pending" || egress.Effect == "" || !strings.Contains(egress.Key, "api.example.com") {
		t.Errorf("the egress request: %+v", egress)
	}
	if egress.Ask == nil || egress.Ask.Context != "need the API" || len(egress.Ask.Options) != 2 || egress.Ask.Recommendation != "once" || egress.Session != "s1" {
		t.Errorf("the sandbox's text: %+v", egress.Ask)
	}
	// The host's words and the sandbox's are not mixed.
	if strings.Contains(egress.Effect, "need the API") {
		t.Errorf("the effect quotes the sandbox: %q", egress.Effect)
	}
	if err := res.Validate(); err != nil {
		t.Error(err)
	}
	one := w.mustCall(daemonproto.MethodRequest, &daemonproto.RequestParams{Env: "alpha", ID: id}).(*daemonproto.RequestResult)
	if one.Request.ID != id || one.Request.Ask == nil {
		t.Errorf("%+v", one)
	}
	if _, err := w.call(daemonproto.ClientCLI, daemonproto.MethodRequest, &daemonproto.RequestParams{Env: "alpha", ID: 99}); kindOf(err) != daemonproto.KindNotFound {
		t.Errorf("no such request: %v", err)
	}
	// The default limit is the protocol's; a limit is honoured.
	res = w.mustCall(daemonproto.MethodRequests, &daemonproto.RequestsParams{Env: "alpha", Limit: 1}).(*daemonproto.RequestsResult)
	if len(res.Requests) != 1 {
		t.Errorf("limit 1 gave %d", len(res.Requests))
	}
}

func TestAnswerOnce(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	id := w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5")})
	res, err := w.answer(id, daemonproto.AnswerParams{Verdict: "allow", Scope: "once"})
	if err != nil || !res.Recorded || res.Grant == "" || res.Request.State != "allowed" || res.Effect == "" {
		t.Fatalf("%+v, %v", res, err)
	}
	if v := w.decide(egressReq("api.example.com", "10.0.0.5")); v != broker.Allowed {
		t.Errorf("the next try: %s", v)
	}
	if v := w.decide(egressReq("api.example.com", "10.0.0.5")); v != broker.Pending {
		t.Errorf("a once grant served twice: %s", v)
	}
	// The same answer again: the first one stands.
	if _, err := w.answer(id, daemonproto.AnswerParams{Verdict: "deny"}); kindOf(err) != daemonproto.KindAlreadyAnswered {
		t.Errorf("second answer: %v", err)
	}
}

func TestAnswerURLOpensItAndDecidesTheRestWithAnHoursGrant(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	w.decide(urlReq("https://example.com/a", "https://example.com"))
	w.decide(urlReq("https://example.com/b", "https://example.com"))
	w.opener.err = errors.New("no browser")
	res, err := w.answer(1, daemonproto.AnswerParams{Verdict: "allow", Scope: "once"})
	if err != nil || !res.Recorded || res.OpenError != "no browser" || len(w.opener.opened) != 1 {
		t.Fatalf("%+v %v %v", res, err, w.opener.opened)
	}
}

func TestAnswerHoursGrantsListsAndRevokes(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	id := w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5")})
	other := w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5"), Session: "other"})
	res, err := w.answer(id, daemonproto.AnswerParams{Verdict: "allow", Scope: "hours", Hours: 8})
	if err != nil || res.Grant == "" || len(res.Also) != 1 || res.Also[0] != other {
		t.Fatalf("%+v, %v", res, err)
	}
	if v := w.decide(egressReq("api.example.com", "10.0.0.5")); v != broker.Allowed {
		t.Errorf("under the grant: %s", v)
	}
	gs := w.mustCall(daemonproto.MethodGrants, &daemonproto.EnvParams{Env: "alpha"}).(*daemonproto.GrantsResult)
	if len(gs.Grants) != 1 || gs.Grants[0].ID != res.Grant || gs.Grants[0].Once || gs.Grants[0].LeftSeconds < 7*3600 || gs.Grants[0].LeftSeconds > 8*3600 {
		t.Fatalf("%+v", gs)
	}
	if err := gs.Validate(); err != nil {
		t.Error(err)
	}
	// permissions carries them too.
	p := w.mustCall(daemonproto.MethodPermissions, &daemonproto.EnvParams{Env: "alpha"}).(*daemonproto.PermissionsResult)
	if len(p.Grants) != 1 || p.Grants[0].ID != res.Grant {
		t.Errorf("permissions grants %+v", p.Grants)
	}
	if _, err := w.call(daemonproto.ClientCLI, daemonproto.MethodRevoke, &daemonproto.RevokeParams{Env: "alpha", Grant: res.Grant}); err != nil {
		t.Fatal(err)
	}
	if v := w.decide(egressReq("api.example.com", "10.0.0.5")); v != broker.Pending {
		t.Errorf("after revoke: %s", v)
	}
	if _, err := w.call(daemonproto.ClientCLI, daemonproto.MethodRevoke, &daemonproto.RevokeParams{Env: "alpha", Grant: res.Grant}); kindOf(err) != daemonproto.KindNotFound {
		t.Errorf("revoking twice: %v", err)
	}
}

// offers is request id's result, offers and all, as a client of kind
// who gets it.
func (w *brokerWorld) offers(who string, id uint64) *daemonproto.RequestResult {
	w.t.Helper()
	res, err := w.call(who, daemonproto.MethodRequest, &daemonproto.RequestParams{Env: "alpha", ID: id})
	if err != nil {
		w.t.Fatalf("request #%d: %v", id, err)
	}
	r := res.(*daemonproto.RequestResult)
	if err := r.Validate(); err != nil {
		w.t.Fatalf("request #%d: %v", id, err)
	}
	return r
}

// alwaysOffer is request id's "always" offer, and its diff as text.
func (w *brokerWorld) alwaysOffer(id uint64) (*daemonproto.Offer, string) {
	w.t.Helper()
	r := w.offers(daemonproto.ClientCLI, id)
	o := r.Offered(daemonproto.ScopeAlways)
	if o == nil {
		w.t.Fatalf("request #%d has no always offer: %+v", id, r)
	}
	return o, strings.Join(o.Diff, "\n")
}

func TestAnswerAlwaysShowsTheDiffThenWritesIt(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	before := w.config()
	id := w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5")})

	o, diff := w.alwaysOffer(id)
	if !strings.Contains(diff, `+ egress_allow = ["*.corp.example", "api.example.com"]`) || !strings.Contains(diff, `- egress_allow = ["*.corp.example"]`) ||
		!strings.Contains(o.Effect, "egress_allow") {
		t.Errorf("the offer: %+v\n%s", o, diff)
	}
	if w.config() != before {
		t.Fatal("offering the diff wrote config.toml")
	}
	// Without the hash of the diff shown, nothing is written.
	if _, err := w.answer(id, daemonproto.AnswerParams{Verdict: "allow", Scope: "always"}); kindOf(err) != daemonproto.KindBadRequest {
		t.Errorf("no confirmation: %v", err)
	}
	if w.config() != before {
		t.Fatal("an answer without a confirmation wrote config.toml")
	}
	if v := w.decide(egressReq("api.example.com", "10.0.0.5")); v != broker.Pending {
		t.Errorf("allowed before it was confirmed: %s", v)
	}

	// A hash of some other diff writes nothing.
	if _, err := w.answer(id, daemonproto.AnswerParams{Verdict: "allow", Scope: "always", ConfirmDiff: strings.Repeat("0", 64)}); kindOf(err) != daemonproto.KindStaleDiff {
		t.Errorf("wrong hash: %v", err)
	}
	if w.config() != before {
		t.Fatal("a wrong hash wrote config.toml")
	}

	// config.toml changes between showing and confirming: the diff the
	// person saw is no longer the change, so nothing is written.
	edited := strings.Replace(before, "egress = true", "egress = true\nmemory = \"8G\"", 1)
	if err := os.WriteFile(w.cfg, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.answer(id, daemonproto.AnswerParams{Verdict: "allow", Scope: "always", ConfirmDiff: o.Hash}); kindOf(err) != daemonproto.KindStaleDiff {
		t.Errorf("stale diff: %v", err)
	}
	if w.config() != edited {
		t.Fatal("a stale diff changed config.toml")
	}
	if r := w.offers(daemonproto.ClientCLI, id); r.Request.State != "pending" {
		t.Errorf("a stale answer decided the request: %+v", r.Request)
	}

	// Offered again, confirmed with the new hash: written exactly as
	// offered, comments kept.
	o, _ = w.alwaysOffer(id)
	res, err := w.answer(id, daemonproto.AnswerParams{Verdict: "allow", Scope: "always", ConfirmDiff: o.Hash})
	if err != nil || !res.Recorded || res.Request.State != "allowed" || res.Request.Scope != "always" || res.DiffHash != o.Hash {
		t.Fatalf("%+v, %v", res, err)
	}
	got := w.config()
	if written := strings.Split(strings.TrimSuffix(lineDiff(edited, got), "\n"), "\n"); strings.Join(written, "\n") != strings.Join(o.Diff, "\n") {
		t.Errorf("written:\n%s\noffered:\n%s", strings.Join(written, "\n"), strings.Join(o.Diff, "\n"))
	}
	// Exactly the one line differs from the file as it was.
	want := strings.Replace(edited, `egress_allow = ["*.corp.example"]`, `egress_allow = ["*.corp.example", "api.example.com"]`, 1)
	if got != want {
		t.Errorf("config.toml now:\n%s\nwant\n%s", got, want)
	}
	for _, kept := range []string{"# my caboose config -- hand edited", "# the outbound proxy", "# a host command", "[alias.deploy]",
		`open_urls_allow = ["https://docs.example.com"]   # docs I read`, `isolation = "vm.default"   # the VM, for the VPN`, "cpus = 4   # a comment"} {
		if !strings.Contains(got, kept) {
			t.Errorf("config.toml lost %q", kept)
		}
	}
	// The very next try is allowed, not filed anew.
	if v := w.decide(egressReq("api.example.com", "10.0.0.5")); v != broker.Allowed {
		t.Errorf("the next request after always: %s", v)
	}
	if n := len(w.mustCall(daemonproto.MethodRequests, &daemonproto.RequestsParams{Env: "alpha"}).(*daemonproto.RequestsResult).Requests); n != 0 {
		t.Errorf("%d requests still pending", n)
	}
}

func TestAnswerAlwaysForAURL(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	w.decide(urlReq("https://example.com/a", "https://example.com"))
	o, diff := w.alwaysOffer(1)
	if !strings.Contains(diff, `"https://example.com"`) {
		t.Fatalf("%+v", o)
	}
	res, err := w.answer(1, daemonproto.AnswerParams{Verdict: "allow", Scope: "always", ConfirmDiff: o.Hash})
	if err != nil || !res.Recorded || len(w.opener.opened) != 1 {
		t.Fatalf("%+v, %v, %v", res, err, w.opener.opened)
	}
	if !strings.Contains(w.config(), `open_urls_allow = ["https://docs.example.com", "https://example.com"]`) {
		t.Errorf("config.toml:\n%s", w.config())
	}
	if v := w.decide(urlReq("https://example.com/b", "https://example.com")); v != broker.Allowed {
		t.Errorf("the next URL: %s", v)
	}
}

// A request's offers: once, hours with its bounds, always with the diff;
// a scope not offered (always, for a URL under hostile) is withheld with
// the reason, and an answer of it is refused.
func TestRequestOffers(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	id := w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5")})
	r := w.offers(daemonproto.ClientWeb, id)
	if len(r.Offers) != 3 || len(r.Withheld) != 0 {
		t.Fatalf("%+v", r)
	}
	if h := r.Offered(daemonproto.ScopeHours); h == nil || h.HoursMin != 1 || h.HoursMax != daemonproto.MaxHours || !strings.Contains(h.Effect, "{hours} hours") {
		t.Errorf("hours: %+v", h)
	}
	if o := r.Offered(daemonproto.ScopeOnce); o == nil || o.Effect == "" || len(o.Diff) != 0 || o.Hash != "" {
		t.Errorf("once: %+v", o)
	}

	hostile := strings.Replace(brokerConfig, `isolation = "vm.default"`, "trust = \"hostile\"\nisolation = \"vm.default\"", 1)
	if err := os.WriteFile(w.cfg, []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.b.Reload(); err != nil {
		t.Fatal(err)
	}
	w.decide(urlReq("https://example.com/a", "https://example.com"))
	list := w.mustCall(daemonproto.MethodRequests, &daemonproto.RequestsParams{Env: "alpha"}).(*daemonproto.RequestsResult)
	var uid uint64
	for _, v := range list.Requests {
		if v.Kind == "open_url" {
			uid = v.ID
		}
	}
	r = w.offers(daemonproto.ClientCLI, uid)
	if r.Offered(daemonproto.ScopeAlways) != nil || r.Offered(daemonproto.ScopeHours) != nil || len(r.Withheld) != 2 ||
		r.Withheld[0].Scope != "hours" || r.Withheld[1].Scope != "always" || !strings.Contains(r.Withheld[1].Why, "hostile") {
		t.Fatalf("hostile URL: %+v", r)
	}
	if _, err := w.answer(uid, daemonproto.AnswerParams{Verdict: "allow", Scope: "always", ConfirmDiff: strings.Repeat("a", 64)}); kindOf(err) != daemonproto.KindBadRequest {
		t.Errorf("always for a hostile URL: %v", err)
	}
	if strings.Contains(w.config(), "https://example.com\"") {
		t.Error("written")
	}
	// Once answered, nothing is offered.
	if _, err := w.answer(id, daemonproto.AnswerParams{Verdict: "deny"}); err != nil {
		t.Fatal(err)
	}
	if r := w.offers(daemonproto.ClientCLI, id); len(r.Offers) != 0 || len(r.Withheld) != 0 {
		t.Errorf("answered: %+v", r)
	}
}

// An "always" offer whose diff would be shown otherwise than written (a
// character made printable) is withheld, since its hash confirms the diff
// as written; the other scopes are still offered. A tab, shown as a
// space, still gets the offer (TestRequestOffersATabbedDiff).
func TestRequestOffersWithholdADiffShownChanged(t *testing.T) {
	for name, line := range map[string]string{
		"invisible": "egress_allow = [\"*.corp.example\"] # zero\u200bwidth",
		"bidi":      "egress_allow = [\"*.corp.example\"] # a\u202eb",
	} {
		t.Run(name, func(t *testing.T) {
			w := newBrokerWorld(t, strings.Replace(brokerConfig, `egress_allow = ["*.corp.example"]`, line, 1))
			id := w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5")})
			r := w.offers(daemonproto.ClientWeb, id)
			if r.Offered(daemonproto.ScopeAlways) != nil || len(r.Withheld) != 1 || r.Withheld[0].Scope != "always" || !strings.Contains(r.Withheld[0].Why, "shown whole") {
				t.Fatalf("%+v", r)
			}
			if r.Offered(daemonproto.ScopeOnce) == nil || r.Offered(daemonproto.ScopeHours) == nil {
				t.Errorf("the other scopes: %+v", r)
			}
		})
	}
}

// A config.toml with a tab in a line the diff shows still gets the
// "always" offer, its tab shown as a space.
func TestRequestOffersATabbedDiff(t *testing.T) {
	w := newBrokerWorld(t, strings.Replace(brokerConfig, `egress_allow = ["*.corp.example"]`, "egress_allow = [\"*.corp.example\"]\t# tabbed", 1))
	id := w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5")})
	r := w.offers(daemonproto.ClientWeb, id)
	o := r.Offered(daemonproto.ScopeAlways)
	if o == nil || len(r.Withheld) != 0 {
		t.Fatalf("%+v", r)
	}
	if !slices.ContainsFunc(o.Diff, func(l string) bool { return strings.Contains(l, "] # tabbed") }) {
		t.Errorf("the tab is not shown as a space: %q", o.Diff)
	}
}

func TestShownAsWritten(t *testing.T) {
	for _, c := range []struct {
		diff string
		want bool
	}{
		{"", true}, {"-a\n+b\n", true}, {"-a\n+b", true}, {"-a\n+\tb\n", true}, {"+a\u202eb\n", false},
		{"+" + strings.Repeat("x", daemonproto.MaxDiff+10) + "\n", false},
	} {
		if got := shownAsWritten(c.diff, daemonproto.DiffLines(c.diff)); got != c.want {
			t.Errorf("%q: %v", c.diff, got)
		}
	}
}

// Who answered is the kind of client the daemon says the call came from,
// for every kind that may answer, whatever the params claim; any other
// kind is refused here too.
func TestAnswerIsRecordedAsTheCaller(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	for _, who := range []string{daemonproto.ClientCLI, daemonproto.ClientWeb, daemonproto.ClientApp} {
		id := w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5"), Session: who})
		liar := map[string]any{"env": "alpha", "id": id, "verdict": "deny", "by": "cli", "decided_by": "dialog", "client": "cli"}
		if who == daemonproto.ClientCLI {
			liar["by"], liar["decided_by"] = "web", "web"
		}
		res, err := w.call(who, daemonproto.MethodAnswer, liar)
		if err != nil {
			t.Fatalf("%s: %v", who, err)
		}
		if got := res.(*daemonproto.AnswerResult).Request.DecidedBy; got != who {
			t.Errorf("%s answered, recorded as %q", who, got)
		}
		if got := w.offers(daemonproto.ClientCLI, id).Request.DecidedBy; got != who {
			t.Errorf("%s answered, stored as %q", who, got)
		}
	}
	id := w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5"), Session: "x"})
	for _, who := range []string{daemonproto.ClientDaemon, "", "dialog", "expiry"} {
		if _, err := w.call(who, daemonproto.MethodAnswer, &daemonproto.AnswerParams{Env: "alpha", ID: id, Verdict: "deny"}); kindOf(err) != daemonproto.KindNotAllowed {
			t.Errorf("%q: %v", who, err)
		}
		if _, err := w.call(who, daemonproto.MethodRevoke, &daemonproto.RevokeParams{Env: "alpha", Grant: "g1"}); kindOf(err) != daemonproto.KindNotAllowed {
			t.Errorf("%q revoke: %v", who, err)
		}
	}
}

func TestFirstAnswerWinsAndDenyGrantsNothing(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	id := w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5")})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, v := range []string{"deny", "allow"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := daemonproto.AnswerParams{Verdict: v}
			if v == "allow" {
				p.Scope = "hours"
				p.Hours = 1
			}
			_, errs[i] = w.answer(id, p)
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("both or neither won: %v, %v", errs[0], errs[1])
	}
	loser := errs[0]
	if loser == nil {
		loser = errs[1]
	}
	if kindOf(loser) != daemonproto.KindAlreadyAnswered {
		t.Errorf("the loser: %v", loser)
	}
	gs := w.mustCall(daemonproto.MethodGrants, &daemonproto.EnvParams{Env: "alpha"}).(*daemonproto.GrantsResult)
	if (errs[0] == nil) != (len(gs.Grants) == 0) {
		t.Errorf("grants %+v after errors %v", gs.Grants, errs)
	}
}

func TestBrokerErrorKinds(t *testing.T) {
	for kind, want := range map[string]string{
		broker.KindNotOwner: daemonproto.KindNotOwner, broker.KindNotFound: daemonproto.KindNotFound,
		broker.KindAlreadyAnswered: daemonproto.KindAlreadyAnswered, broker.KindStaleDiff: daemonproto.KindStaleDiff,
		broker.KindBadRequest: daemonproto.KindBadRequest, broker.KindUnavailable: daemonproto.KindBrokerUnavailable,
		broker.KindTooManyWaits: daemonproto.KindBusy, "other": daemonproto.KindInternal,
	} {
		err := brokerError(&broker.Error{Kind: kind, Msg: "m\x1b"})
		if daemonproto.Kind(err) != want || strings.Contains(err.Error(), "\x1b") {
			t.Errorf("%s -> %v", kind, err)
		}
	}
	if daemonproto.Kind(brokerError(errors.New("x"))) != daemonproto.KindInternal {
		t.Error("a plain error is internal")
	}
}

func TestPermissionsReport(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	w.mods = func() (agentproto.ModsInfo, bool) { return agentproto.ModsInfo{Sessions: 3, WithMod: 2}, true }
	p := w.mustCall(daemonproto.MethodPermissions, &daemonproto.EnvParams{Env: "alpha"}).(*daemonproto.PermissionsResult)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Env != "alpha" || p.Trust != "fallible" || p.Isolation != "vm" || p.Profile != "vm.default" {
		t.Errorf("%+v", p)
	}
	if len(p.Aliases) != 1 || p.Aliases[0].Name != "deploy" || p.Aliases[0].Policy != "ask" || strings.Join(p.Aliases[0].Command, " ") != "/usr/bin/true now" {
		t.Errorf("aliases %+v", p.Aliases)
	}
	if !p.Egress.On || len(p.Egress.Allow) != 1 || p.Egress.Ports != "22 80 443" || p.Egress.Default == "" || p.Egress.Network == "" {
		t.Errorf("egress %+v", p.Egress)
	}
	if len(p.OpenURLs.Allow) != 1 || p.OpenURLs.Mode != "ask" || p.OpenURLs.Why != "" {
		t.Errorf("open urls %+v", p.OpenURLs)
	}
	if p.Mods == nil || p.Mods.Sessions != 3 || p.Mods.WithMod != 2 {
		t.Errorf("mods %+v", p.Mods)
	}
	if len(p.Roots) == 0 {
		t.Error("no roots")
	}
}

// The report says when the sandbox was created with other roots, writable
// parts, trust or network than config.toml now gives, which only a
// restart changes; nothing when they agree or there is no sandbox.
func TestPermissionsDrift(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	drift := func() []string {
		t.Helper()
		p := w.mustCall(daemonproto.MethodPermissions, &daemonproto.EnvParams{Env: "alpha"}).(*daemonproto.PermissionsResult)
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		return p.Drift
	}
	if d := drift(); len(d) != 0 {
		t.Errorf("no sandbox, drift %q", d)
	}
	app, err := w.l.newApp("alpha")
	if err != nil {
		t.Fatal(err)
	}
	roots := app.Cfg.Roots
	if len(roots) == 0 {
		t.Fatal("no roots configured")
	}
	var mounts []backend.Mount
	for _, r := range roots {
		mounts = append(mounts, backend.Mount{Source: r.Host, Target: r.Container})
	}
	agree := map[string]string{assets.LabelRoots: rootsLabel(roots), assets.LabelWritable: writableLabel(roots),
		assets.LabelTrust: config.TrustFallible, assets.LabelIsolation: isolationVM, assets.LabelNetwork: app.networkLabel()}
	w.box.Status, w.box.SandboxLabels, w.box.SandboxMounts = "running", agree, mounts
	if d := drift(); len(d) != 0 {
		t.Errorf("a sandbox as configured, drift %q", d)
	}

	labels := maps.Clone(agree)
	labels[assets.LabelTrust] = config.TrustHostile
	w.box.SandboxLabels = labels
	if d := drift(); len(d) != 1 || !strings.Contains(d[0], "trust hostile; the configuration says fallible") {
		t.Errorf("trust drift %q", d)
	}

	labels = maps.Clone(agree)
	labels[assets.LabelRoots] = `["/work/old"]`
	w.box.SandboxLabels, w.box.SandboxMounts = labels, []backend.Mount{{Source: "/h/old", Target: "/work/old"}}
	if d := drift(); len(d) != 1 || !strings.Contains(d[0], "mounts /h/old at /work/old") {
		t.Errorf("roots drift %q", d)
	}

	labels = maps.Clone(agree)
	flipped := slices.Clone(roots)
	for i := range flipped {
		if slices.Equal(flipped[i].Writable, []string{"."}) {
			flipped[i].Writable = nil
		} else {
			flipped[i].Writable = []string{"."}
		}
	}
	labels[assets.LabelWritable] = writableLabel(flipped)
	labels[assets.LabelNetwork] = "elsewhere\x1b[2J"
	w.box.SandboxLabels, w.box.SandboxMounts = labels, mounts
	d := drift()
	if len(d) != 2 || !strings.Contains(d[0], "config.toml now makes") || !strings.Contains(d[1], "network elsewhere?[2J") {
		t.Errorf("writable and network drift %q", d)
	}
}

// countBox counts what is asked of a sandbox's labels and mounts.
type countBox struct {
	*backendtest.Fake
	labels, mounts int
}

func (c *countBox) Labels() (map[string]string, error) { c.labels++; return c.Fake.Labels() }
func (c *countBox) Mounts() ([]backend.Mount, error)   { c.mounts++; return c.Fake.Mounts() }

// The drift checks read the sandbox's labels and mounts once between
// them, and leave the app's backend as it was.
func TestRestartDriftAsksOnce(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	app, err := w.l.newApp("alpha")
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{assets.LabelRoots: `["/work/old"]`, assets.LabelTrust: config.TrustHostile, assets.LabelIsolation: isolationVM}
	box := &countBox{Fake: &backendtest.Fake{Status: "running", SandboxLabels: labels, SandboxMounts: []backend.Mount{{Source: "/h/old", Target: "/work/old"}}}}
	app.Backend = box
	if d := app.restartDrift(); len(d) < 2 {
		t.Errorf("drift %q", d)
	}
	if box.labels != 1 || box.mounts != 1 {
		t.Errorf("asked for labels %d times, mounts %d times", box.labels, box.mounts)
	}
	if app.Backend != backend.Backend(box) {
		t.Error("the app's backend was not put back")
	}
}

func TestPermissionsOfHostile(t *testing.T) {
	c := &config.Config{Env: "e", Trust: config.TrustHostile, HostExec: true, OpenURLs: "allow", OpenURLsAllow: []string{"https://x"},
		Isolation: config.KindVM, Profile: "vm.default", Egress: true, EgressDeny: []string{"evil.example"},
		Roots: []config.Root{{Name: "a", Host: "/a", Container: "/work/a", Writable: []string{"."}}, {Name: "b", Host: "/b", Container: "/work/b"}}}
	p := permissionsOf(c)
	if p.HostExec.Effective || !p.HostExec.Configured || !strings.Contains(p.HostExec.Why, "hostile") {
		t.Errorf("host exec %+v", p.HostExec)
	}
	if !strings.Contains(p.OpenURLs.Why, "asks") || !strings.Contains(p.Egress.Default, "hostile") || p.Egress.Deny[0] != "evil.example" {
		t.Errorf("%+v %+v", p.OpenURLs, p.Egress)
	}
	if p.Roots[0].ReadOnly || !p.Roots[1].ReadOnly {
		t.Errorf("roots %+v", p.Roots)
	}
	fall := permissionsOf(&config.Config{Env: "e", Trust: config.TrustFallible, HostExec: true})
	if !fall.HostExec.Effective || fall.HostExec.Why != "" || fall.Isolation != "container" {
		t.Errorf("fallible %+v", fall)
	}
	off := permissionsOf(&config.Config{Env: "e", Trust: config.TrustFallible})
	if off.HostExec.Effective || off.HostExec.Why == "" {
		t.Errorf("host exec off %+v", off.HostExec)
	}
}

// A handcrafted running link: the daemon finds its broker without a hook,
// and status counts its pending requests.
func TestDaemonLinksFindsTheRunningLinksBroker(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	w.l.brokerHook = nil
	e := w.l.env("alpha")
	e.mu.Lock()
	e.cur = &linkInstance{runner: &linkRunner{broker: w.b}}
	e.state = daemonproto.LinkRunning
	e.mu.Unlock()
	t.Cleanup(func() { e.mu.Lock(); e.cur = nil; e.mu.Unlock() })

	if st := w.l.Envs(); len(st) != 1 || st[0].Pending != 0 {
		t.Fatalf("%+v", st)
	}
	w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5")})
	w.decide(urlReq("https://example.com/a", "https://example.com"))
	if st := w.l.Envs(); st[0].Pending != 2 {
		t.Errorf("pending %d", st[0].Pending)
	}
	res, err := w.l.Call(t.Context(), daemonproto.ClientCLI, daemonproto.MethodRequests, "alpha", []byte(`{"env":"alpha"}`))
	if err != nil || len(res.(*daemonproto.RequestsResult).Requests) != 2 {
		t.Fatalf("%v %v", res, err)
	}
	if _, err := w.answer(1, daemonproto.AnswerParams{Verdict: "deny"}); err != nil {
		t.Fatal(err)
	}
	if st := w.l.Envs(); st[0].Pending != 1 {
		t.Errorf("pending %d after a deny", st[0].Pending)
	}
	_ = time.Second
}

// edit_egress through the daemon's broker service, on a real config.toml:
// the diff without writing, then the write with its hash, line by line,
// comments kept, and the next connection decided by it at once.
func TestEditEgressService(t *testing.T) {
	w := newBrokerWorld(t, brokerConfig)
	r := egressReq("git.vpn.example", "10.1.2.3")
	if v := w.decide(r); v != broker.Pending {
		t.Fatalf("before: %s", v)
	}
	before, _ := os.ReadFile(w.cfg)
	p := daemonproto.EditEgressParams{Env: "alpha", AddAllow: []string{"git.vpn.example"}, AddDeny: []string{"10.9.0.0/16"}}
	res, err := w.call(daemonproto.ClientWeb, daemonproto.MethodEditEgress, p)
	if err != nil {
		t.Fatal(err)
	}
	shown := res.(*daemonproto.EditEgressResult)
	if shown.Written || shown.Validate() != nil {
		t.Fatalf("shown %+v", shown)
	}
	if got, _ := os.ReadFile(w.cfg); string(got) != string(before) {
		t.Fatal("config.toml changed without a confirmation")
	}
	diff := strings.Join(shown.Diff, "\n")
	if !strings.Contains(diff, `+ egress_allow = ["*.corp.example", "git.vpn.example"]`) || !strings.Contains(diff, `+ egress_deny = ["10.9.0.0/16"]`) {
		t.Errorf("diff:\n%s", diff)
	}
	if !strings.Contains(shown.Effect, "allows connections to git.vpn.example from environment alpha") {
		t.Errorf("effect %q", shown.Effect)
	}

	// A wrong hash writes nothing.
	p.ConfirmDiff = strings.Repeat("0", 64)
	if _, err := w.call(daemonproto.ClientWeb, daemonproto.MethodEditEgress, p); kindOf(err) != daemonproto.KindStaleDiff {
		t.Fatalf("wrong hash: %v", err)
	}
	p.ConfirmDiff = shown.Hash
	res, err = w.call(daemonproto.ClientWeb, daemonproto.MethodEditEgress, p)
	if err != nil {
		t.Fatal(err)
	}
	if !res.(*daemonproto.EditEgressResult).Written {
		t.Fatalf("not written: %+v", res)
	}
	got, _ := os.ReadFile(w.cfg)
	for _, want := range []string{"# my caboose config -- hand edited", "cpus = 4   # a comment on a line the edit leaves alone", `egress_allow = ["*.corp.example", "git.vpn.example"]`, `egress_deny = ["10.9.0.0/16"]`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("config.toml lacks %q:\n%s", want, got)
		}
	}
	if v := w.decide(r); v != broker.Allowed {
		t.Errorf("after: %s", v)
	}
	if lines := strings.Count(readOr(filepath.Join(w.envDir, "decisions.log")), `"event":"egress-edit"`); lines != 1 {
		t.Errorf("%d egress-edit lines in decisions.log", lines)
	}
	// Removing what is not there is refused; outside vm, all is.
	if _, err := w.call(daemonproto.ClientCLI, daemonproto.MethodEditEgress, daemonproto.EditEgressParams{Env: "alpha", RemoveAllow: []string{"absent.example"}}); kindOf(err) != daemonproto.KindBadRequest {
		t.Errorf("absent removal: %v", err)
	}
	if _, err := w.call("daemon", daemonproto.MethodEditEgress, daemonproto.EditEgressParams{Env: "alpha", AddAllow: []string{"x.example"}}); kindOf(err) != daemonproto.KindNotAllowed {
		t.Errorf("a daemon kind: %v", err)
	}
}

// An egress edit whose diff would be shown otherwise than written (a
// character made printable) is refused, shown or confirmed with the hash
// of the diff as written, and config.toml is left alone; a tab, shown as
// a space, is let through as for an "always" offer.
func TestEditEgressRefusesADiffShownChanged(t *testing.T) {
	line := "egress_allow = [\"*.corp.example\"] # a\u202eb"
	w := newBrokerWorld(t, strings.Replace(brokerConfig, `egress_allow = ["*.corp.example"]`, line, 1))
	before, _ := os.ReadFile(w.cfg)
	p := daemonproto.EditEgressParams{Env: "alpha", AddAllow: []string{"git.vpn.example"}}
	if _, err := w.call(daemonproto.ClientWeb, daemonproto.MethodEditEgress, p); kindOf(err) != daemonproto.KindBrokerUnavailable || !strings.Contains(err.Error(), "cannot be shown") {
		t.Fatalf("shown: %v", err)
	}
	// A confirmation is refused before its hash is compared: a client
	// could have hashed the diff as written itself.
	p.ConfirmDiff = strings.Repeat("a", 64)
	if _, err := w.call(daemonproto.ClientWeb, daemonproto.MethodEditEgress, p); kindOf(err) != daemonproto.KindBrokerUnavailable {
		t.Fatalf("confirmed: %v", err)
	}
	if got, _ := os.ReadFile(w.cfg); string(got) != string(before) {
		t.Fatalf("config.toml changed:\n%s", got)
	}

	tabbed := newBrokerWorld(t, strings.Replace(brokerConfig, `egress_allow = ["*.corp.example"]`, "egress_allow = [\"*.corp.example\"]\t# tabbed", 1))
	res, err := tabbed.call(daemonproto.ClientWeb, daemonproto.MethodEditEgress, daemonproto.EditEgressParams{Env: "alpha", AddAllow: []string{"git.vpn.example"}})
	if err != nil {
		t.Fatalf("tabbed: %v", err)
	}
	if r := res.(*daemonproto.EditEgressResult); r.Hash == "" || !slices.ContainsFunc(r.Diff, func(l string) bool { return strings.Contains(l, "] # tabbed") }) {
		t.Errorf("tabbed: %+v", r)
	}
}

func TestEditEgressServiceOutsideVM(t *testing.T) {
	w := newBrokerWorld(t, strings.Replace(brokerConfig, `isolation = "vm.default"   # the VM, for the VPN`, "isolation = \"container.default\"\n\n[container.default]", 1))
	_, err := w.call(daemonproto.ClientCLI, daemonproto.MethodEditEgress, daemonproto.EditEgressParams{Env: "alpha", AddAllow: []string{"x.example"}})
	if kindOf(err) != daemonproto.KindBadRequest || !strings.Contains(err.Error(), "only under vm") {
		t.Fatalf("got %v", err)
	}
}

// readOr is the file at path, "" when it cannot be read.
func readOr(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}
