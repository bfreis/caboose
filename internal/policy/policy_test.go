package policy

import (
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/config"
)

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func addrs(ss ...string) []netip.Addr {
	var out []netip.Addr
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

func mustAllow(t testing.TB, s string) Allow {
	t.Helper()
	a, err := ParseAllow(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustPorts(t testing.TB, s string) config.PortSet {
	t.Helper()
	p, err := config.ParsePortsOf("egress_ports", s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func live(kind Kind, key, id string) Grant {
	return Grant{ID: id, Kind: kind, Key: key, Expires: t0.Add(time.Hour)}
}

type decideCase struct {
	name string
	p    Policy
	r    Request
	want Verdict
	rule string
}

func runCases(t *testing.T, cases []decideCase) {
	t.Helper()
	for _, c := range cases {
		d := Decide(c.p, c.r, t0)
		if d.Verdict != c.want || c.rule != "" && d.Rule != c.rule {
			t.Errorf("%s: got %v %q (%s), want %v %q", c.name, d.Verdict, d.Rule, d.Reason, c.want, c.rule)
		}
		if d.Reason == "" {
			t.Errorf("%s: no reason", c.name)
		}
	}
}

func TestVerdict(t *testing.T) {
	var v Verdict
	if v != VerdictDeny || v.String() != "deny" || VerdictAllow.String() != "allow" || VerdictAsk.String() != "ask" || Verdict(9).String() != "Verdict(9)" {
		t.Error("verdict names, or a zero that is not deny")
	}
}

func TestUnknownKind(t *testing.T) {
	p := Policy{Trust: TrustFallible, HostExec: true, OpenURLs: OpenAllow}
	for _, k := range []Kind{"", "ALIAS", "host-exec", "openurl", "egress "} {
		runCases(t, []decideCase{{string(k), p, Request{Kind: k, Alias: "x", Host: "x"}, VerdictDeny, "deny:unknown-kind"}})
		if GrantKey(Request{Kind: k, Alias: "x", Host: "x", Origin: "x"}) != "" {
			t.Errorf("GrantKey of kind %q", k)
		}
	}
}

func TestGrantKey(t *testing.T) {
	for r, want := range map[*Request]string{
		{Kind: KindAlias, Alias: "build"}:                  "alias:build",
		{Kind: KindEgress, Host: "api.example"}:            "egress:api.example",
		{Kind: KindOpenURL, Origin: "https://x.example"}:   "open_url:https://x.example",
		{Kind: KindHostExec}:                               "",
		{Kind: KindAlias}:                                  "",
		{Kind: KindEgress, Alias: "x"}:                     "",
		{Kind: KindOpenURL, URL: "https://x.example/"}:     "",
		{Kind: "nope", Alias: "x", Host: "x", Origin: "x"}: "",
	} {
		if got := GrantKey(*r); got != want {
			t.Errorf("GrantKey(%+v) = %q, want %q", *r, got, want)
		}
	}
}

func TestDecideHostExec(t *testing.T) {
	r := Request{Kind: KindHostExec}
	runCases(t, []decideCase{
		{"hostile on", Policy{Trust: TrustHostile, HostExec: true}, r, VerdictDeny, "deny:hostile-host-exec"},
		{"empty trust", Policy{HostExec: true}, r, VerdictDeny, "deny:hostile-host-exec"},
		{"odd trust", Policy{Trust: "Fallible", HostExec: true}, r, VerdictDeny, "deny:hostile-host-exec"},
		{"off", Policy{Trust: TrustFallible}, r, VerdictDeny, "deny:host_exec-off"},
		{"on", Policy{Trust: TrustFallible, HostExec: true}, r, VerdictAllow, "allow:host_exec"},
	})
	d := Decide(Policy{Trust: TrustFallible}, r, t0)
	if d.Reason != agentproto.HostExecOff {
		t.Errorf("off: %q", d.Reason)
	}
	if d := Decide(Policy{Trust: TrustHostile, HostExec: true}, r, t0); !strings.Contains(d.Reason, "trust") {
		t.Errorf("hostile does not name trust: %q", d.Reason)
	}
}

func TestDecideCheckpoint(t *testing.T) {
	r := Request{Kind: KindCheckpoint}
	runCases(t, []decideCase{
		{"fallible allow", Policy{Trust: TrustFallible, CheckpointRequests: CheckpointAllow}, r, VerdictAllow, "allow:checkpoint_requests"},
		{"hostile allow", Policy{Trust: TrustHostile, CheckpointRequests: CheckpointAllow}, r, VerdictAllow, "allow:checkpoint_requests"},
		{"fallible off", Policy{Trust: TrustFallible, CheckpointRequests: CheckpointOff}, r, VerdictDeny, "deny:checkpoint_requests-off"},
		{"hostile off", Policy{Trust: TrustHostile, CheckpointRequests: CheckpointOff}, r, VerdictDeny, "deny:checkpoint_requests-off"},
		{"unset", Policy{Trust: TrustFallible}, r, VerdictDeny, "deny:checkpoint_requests-policy"},
		{"odd", Policy{Trust: TrustFallible, CheckpointRequests: "Allow"}, r, VerdictDeny, "deny:checkpoint_requests-policy"},
	})
	if k := GrantKey(r); k != "" {
		t.Errorf("GrantKey = %q", k)
	}
	if e := Effect(Policy{}, r); !strings.Contains(e, "checkpoint") {
		t.Errorf("Effect = %q", e)
	}
}

func aliasPolicy(trust Trust, pol string) Policy {
	return Policy{
		Trust:    trust,
		Writable: []string{"/Users/me/dev/proj", "/Users/me/dev/other/out"},
		Aliases: map[string]Alias{
			"build": {
				Name:    "build",
				Command: []string{"/usr/bin/make", "{target}", "-C", "/Users/me/dev/proj"},
				Cwd:     "/Users/me/dev/proj",
				Params:  map[string][]string{"target": {"all", "test"}},
				Policy:  pol,
			},
		},
	}
}

func aliasReq() Request {
	return Request{
		Kind: KindAlias, Alias: "build", Args: map[string]string{"target": "test"},
		Command: "/usr/bin/make", Cwd: "/Users/me/dev/proj", ArgPaths: []string{"/Users/me/dev/proj"},
	}
}

func TestDecideAlias(t *testing.T) {
	with := func(f func(*Request)) Request {
		r := aliasReq()
		f(&r)
		return r
	}
	safe := func(r *Request) { r.Cwd, r.ArgPaths = "/opt/x", []string{"/opt/y"} }
	many := map[string]string{}
	for i := range MaxArgs + 1 {
		many[string(rune('a'+i%26))+strings.Repeat("x", i)] = "v"
	}
	grantP := aliasPolicy(TrustFallible, AliasAsk)
	grantP.Grants = []Grant{live(KindAlias, "alias:build", "g1")}
	outside := func(p Policy) Policy {
		a := p.Aliases["build"]
		a.Cwd = "/opt/x"
		p.Aliases = map[string]Alias{"build": a}
		return p
	}
	hostileGrant := outside(aliasPolicy(TrustHostile, AliasAsk))
	hostileGrant.Grants = grantP.Grants
	linked := aliasPolicy(TrustFallible, "")
	linked.Aliases["build"] = func() Alias {
		a := linked.Aliases["build"]
		a.Command = append([]string{"/Users/me/dev/proj/tools/../make"}, a.Command[1:]...)
		return a
	}()
	hostileCwd := aliasPolicy(TrustHostile, "")
	hostileCwd.Aliases["build"] = func() Alias {
		a := hostileCwd.Aliases["build"]
		a.Cwd = "/users/me/DEV/proj/sub"
		return a
	}()
	wide := aliasPolicy(TrustFallible, "")
	wide.Aliases["build"] = func() Alias {
		a := wide.Aliases["build"]
		a.Params = map[string][]string{"target": {"all"}}
		for i := range MaxArgs + 4 {
			a.Params[fmt.Sprintf("p%d", i)] = []string{"v"}
		}
		return a
	}()
	runCases(t, []decideCase{
		{"no such alias", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Alias = "nope" }), VerdictDeny, "deny:no-alias"},
		{"empty alias name", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Alias = "" }), VerdictDeny, "deny:no-alias"},
		{"policy deny", aliasPolicy(TrustFallible, AliasDeny), aliasReq(), VerdictDeny, "deny:alias-deny"},
		{"policy odd", aliasPolicy(TrustFallible, "Allow"), aliasReq(), VerdictDeny, "deny:alias-policy"},
		{"unknown arg", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Args = map[string]string{"x": "all"} }), VerdictDeny, "deny:alias-args"},
		{"value not in enum", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Args = map[string]string{"target": "clean"} }), VerdictDeny, "deny:alias-args"},
		{"too many args", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Args = many }), VerdictDeny, "deny:alias-args"},
		{"relative command", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Command = "usr/bin/make" }), VerdictDeny, "deny:alias-unresolved"},
		{"unclean command", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Command = "/usr/bin/../bin/make" }), VerdictDeny, "deny:alias-unresolved"},
		{"empty command", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Command = "" }), VerdictDeny, "deny:alias-unresolved"},
		{"relative cwd", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Cwd = "proj" }), VerdictDeny, "deny:alias-unresolved"},
		{"missing cwd", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Cwd = "" }), VerdictDeny, "deny:alias-unresolved"},
		{"unclean arg path", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.ArgPaths = []string{"/a/./b"} }), VerdictDeny, "deny:alias-unresolved"},
		{"missing arg path", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.ArgPaths = nil }), VerdictDeny, "deny:alias-unresolved"},
		{"command writable", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Command = "/Users/me/dev/proj/bin/make" }), VerdictDeny, "deny:alias-command-writable"},
		{"command writable, case", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Command = "/users/ME/Dev/PROJ/make" }), VerdictDeny, "deny:alias-command-writable"},
		{"command writable, root", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Command = "/Users/me/dev/other/out" }), VerdictDeny, "deny:alias-command-writable"},
		{"command writable, grant", grantP, with(func(r *Request) { r.Command = "/Users/me/dev/proj/make" }), VerdictDeny, "deny:alias-command-writable"},
		{"command beside writable", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Command = "/Users/me/dev/project/make" }), VerdictAllow, "allow:alias"},
		{"hostile cwd writable", aliasPolicy(TrustHostile, ""), with(func(r *Request) { r.ArgPaths = []string{"/opt/y"} }), VerdictDeny, "deny:hostile-alias-writable"},
		{"hostile arg writable", aliasPolicy(TrustHostile, ""), with(func(r *Request) { r.Cwd = "/opt/x" }), VerdictDeny, "deny:hostile-alias-writable"},
		{"odd trust arg writable", aliasPolicy("", ""), with(func(r *Request) { r.Cwd = "/opt/x" }), VerdictDeny, "deny:hostile-alias-writable"},
		{"hostile, nothing writable", outside(aliasPolicy(TrustHostile, "")), with(safe), VerdictAllow, "allow:alias"},
		{"fallible cwd writable", aliasPolicy(TrustFallible, ""), aliasReq(), VerdictAllow, "allow:alias"},
		{"policy allow", aliasPolicy(TrustFallible, AliasAllow), aliasReq(), VerdictAllow, "allow:alias"},
		{"missing arg takes the first", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.Args = nil }), VerdictAllow, "allow:alias"},
		{"ask", aliasPolicy(TrustFallible, AliasAsk), aliasReq(), VerdictAsk, "ask:alias"},
		{"ask, grant", grantP, aliasReq(), VerdictAllow, "grant:g1"},
		{"hostile ask, grant", hostileGrant, with(safe), VerdictAllow, "grant:g1"},
		{"configured command writable", linked, with(func(r *Request) { r.Command = "/bin/sh" }), VerdictDeny, "deny:alias-command-writable"},
		{"command through writable link", grantP, with(func(r *Request) { r.CommandVia = []string{"/usr/local/bin/make", "/Users/me/dev/proj/make"} }), VerdictDeny, "deny:alias-command-writable"},
		{"command through outside link", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.CommandVia = []string{"/usr/local/bin/make"} }), VerdictAllow, "allow:alias"},
		{"relative link", aliasPolicy(TrustFallible, ""), with(func(r *Request) { r.CommandVia = []string{"bin/make"} }), VerdictDeny, "deny:alias-unresolved"},
		{"hostile configured cwd writable, resolved outside", hostileCwd, with(safe), VerdictDeny, "deny:hostile-alias-writable"},
		{"most args", wide, with(func(r *Request) { r.Args = wideArgs(MaxArgs) }), VerdictAllow, "allow:alias"},
		{"one arg past the most", wide, with(func(r *Request) { r.Args = wideArgs(MaxArgs + 1) }), VerdictDeny, "deny:alias-args"},
	})
}

func wideArgs(n int) map[string]string {
	m := map[string]string{}
	for i := range n {
		m[fmt.Sprintf("p%d", i)] = "v"
	}
	return m
}

func TestWritableEntries(t *testing.T) {
	r := aliasReq()
	r.Cwd, r.ArgPaths = "/opt/x", []string{"/opt/y"}
	for _, c := range []struct {
		w    []string
		want Verdict
	}{
		{[]string{""}, VerdictAllow},
		{[]string{"/"}, VerdictDeny},
		{[]string{"usr"}, VerdictDeny}, // a relative entry holds everything
		{[]string{"/usr/bin/"}, VerdictDeny},
		{[]string{"/usr/bin/make/x"}, VerdictAllow},
		{[]string{"/USR"}, VerdictDeny},
		{[]string{"/usr/b"}, VerdictAllow},
	} {
		p := aliasPolicy(TrustFallible, "")
		p.Writable = c.w
		if d := Decide(p, r, t0); d.Verdict != c.want {
			t.Errorf("writable %q: %v %s", c.w, d.Verdict, d.Reason)
		}
	}
}

func TestGrantMatching(t *testing.T) {
	p := aliasPolicy(TrustFallible, AliasAsk)
	r := aliasReq()
	for _, c := range []struct {
		name string
		g    Grant
		want Verdict
	}{
		{"live", live(KindAlias, "alias:build", "g"), VerdictAllow},
		{"once", Grant{ID: "g", Kind: KindAlias, Key: "alias:build", Once: true, Exact: `"target"="test"`, Expires: t0.Add(time.Minute)}, VerdictAllow},
		// A once grant is for the request shown: other arguments ask.
		{"once, other args", Grant{ID: "g", Kind: KindAlias, Key: "alias:build", Once: true, Exact: `"target"="prod"`, Expires: t0.Add(time.Minute)}, VerdictAsk},
		{"once, no args", Grant{ID: "g", Kind: KindAlias, Key: "alias:build", Once: true, Expires: t0.Add(time.Minute)}, VerdictAsk},
		{"expired", Grant{ID: "g", Kind: KindAlias, Key: "alias:build", Expires: t0}, VerdictAsk},
		{"zero expiry", Grant{ID: "g", Kind: KindAlias, Key: "alias:build"}, VerdictAsk},
		{"wrong kind", live(KindEgress, "alias:build", "g"), VerdictAsk},
		{"prefix key", live(KindAlias, "alias:buil", "g"), VerdictAsk},
		{"longer key", live(KindAlias, "alias:build2", "g"), VerdictAsk},
		{"empty key", live(KindAlias, "", "g"), VerdictAsk},
	} {
		p.Grants = []Grant{c.g}
		d := Decide(p, r, t0)
		if d.Verdict != c.want || (d.Verdict == VerdictAllow) != (d.Grant == "g") {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
}

// A once grant for a connection is for its port; a timed one for every
// port of the host.
func TestEgressOnceGrantHoldsThePort(t *testing.T) {
	p := egressPolicy(t, TrustHostile, "", "")
	r := Request{Kind: KindEgress, Host: "api.example", Port: 443, Addrs: addrs("140.82.112.3")}
	other := r
	other.Port = 22
	p.Grants = []Grant{{ID: "g", Kind: KindEgress, Key: "egress:api.example", Once: true, Exact: ExactKey(r), Expires: t0.Add(time.Minute)}}
	if d := Decide(p, r, t0); d.Verdict != VerdictAllow || d.Grant != "g" {
		t.Errorf("the port allowed: %+v", d)
	}
	if d := Decide(p, other, t0); d.Verdict != VerdictAsk {
		t.Errorf("another port: %+v", d)
	}
	p.Grants[0].Once = false
	if d := Decide(p, other, t0); d.Verdict != VerdictAllow {
		t.Errorf("a timed grant, another port: %+v", d)
	}
	// And for a private address, held exactly too.
	pr := Request{Kind: KindEgress, Host: "db.corp.example", Port: 22, Addrs: addrs("10.0.0.5")}
	p.Grants = []Grant{{ID: "g", Kind: KindEgress, Key: "egress:db.corp.example", Once: true, Exact: "443", Addrs: addrs("10.0.0.5"), Expires: t0.Add(time.Minute)}}
	if d := Decide(p, pr, t0); d.Verdict != VerdictAsk {
		t.Errorf("private, another port: %+v", d)
	}
}

func TestExactKey(t *testing.T) {
	a := Request{Kind: KindAlias, Args: map[string]string{"b": "2", "a": "1"}}
	b := Request{Kind: KindAlias, Args: map[string]string{"a": "1", "b": "2"}}
	if ExactKey(a) != ExactKey(b) || ExactKey(a) != `"a"="1" "b"="2"` {
		t.Errorf("%q, %q", ExactKey(a), ExactKey(b))
	}
	// No splicing one argument into two.
	c := Request{Kind: KindAlias, Args: map[string]string{"a": `1" "b"="2`}}
	if ExactKey(c) == ExactKey(a) {
		t.Errorf("spliced: %q", ExactKey(c))
	}
	if ExactKey(Request{Kind: KindEgress, Port: 22}) != "22" || ExactKey(Request{Kind: KindOpenURL, Origin: "https://x"}) != "" {
		t.Error("egress, url")
	}
}

func TestDecideOpenURL(t *testing.T) {
	r := Request{Kind: KindOpenURL, URL: "https://Docs.example/a?b", Origin: "https://docs.example"}
	g := []Grant{live(KindOpenURL, "open_url:https://docs.example", "g7")}
	allowList := []string{"https://docs.example"}
	runCases(t, []decideCase{
		{"no url", Policy{Trust: TrustFallible, OpenURLs: OpenAllow}, Request{Kind: KindOpenURL, Origin: "https://x"}, VerdictDeny, "deny:open-url-bad"},
		{"bare scheme origin", Policy{Trust: TrustFallible, OpenURLs: OpenAsk, Grants: []Grant{live(KindOpenURL, "open_url:https:", "g9")}}, Request{Kind: KindOpenURL, URL: "https://x.example/", Origin: "https:"}, VerdictDeny, "deny:open-url-bad"},
		{"origin with a path", Policy{Trust: TrustFallible, OpenURLs: OpenAllow}, Request{Kind: KindOpenURL, URL: "https://x.example/a", Origin: "https://x.example/"}, VerdictDeny, "deny:open-url-bad"},
		{"origin not lower case", Policy{Trust: TrustFallible, OpenURLs: OpenAllow}, Request{Kind: KindOpenURL, URL: "https://X.example/", Origin: "https://X.example"}, VerdictDeny, "deny:open-url-bad"},
		{"no origin", Policy{Trust: TrustFallible, OpenURLs: OpenAllow}, Request{Kind: KindOpenURL, URL: "https://x/"}, VerdictDeny, "deny:open-url-bad"},
		{"other origin", Policy{Trust: TrustFallible, OpenURLs: OpenAllow}, Request{Kind: KindOpenURL, URL: "https://docs.example.evil/", Origin: "https://docs.example"}, VerdictDeny, "deny:open-url-bad"},
		{"off", Policy{Trust: TrustFallible, OpenURLs: OpenOff, OpenURLAllow: allowList, Grants: g}, r, VerdictDeny, "deny:open_urls-off"},
		{"hostile off", Policy{Trust: TrustHostile, OpenURLs: OpenOff}, r, VerdictDeny, "deny:open_urls-off"},
		{"hostile allow", Policy{Trust: TrustHostile, OpenURLs: OpenAllow, OpenURLAllow: allowList, Grants: g}, r, VerdictAsk, "ask:hostile"},
		{"odd trust allow", Policy{Trust: "trusted", OpenURLs: OpenAllow}, r, VerdictAsk, "ask:hostile"},
		{"allow", Policy{Trust: TrustFallible, OpenURLs: OpenAllow}, r, VerdictAllow, "allow:open_urls"},
		{"allowed origin", Policy{Trust: TrustFallible, OpenURLs: OpenAsk, OpenURLAllow: allowList}, r, VerdictAllow, "allow:open_urls_allow"},
		{"origin not listed", Policy{Trust: TrustFallible, OpenURLs: OpenAsk, OpenURLAllow: []string{"https://docs.example:8443", "http://docs.example"}}, r, VerdictAsk, "ask:open_urls"},
		{"grant", Policy{Trust: TrustFallible, OpenURLs: OpenAsk, Grants: g}, r, VerdictAllow, "grant:g7"},
		{"ask", Policy{Trust: TrustFallible, OpenURLs: OpenAsk}, r, VerdictAsk, "ask:open_urls"},
		{"empty setting", Policy{Trust: TrustFallible, OpenURLAllow: allowList, Grants: g}, r, VerdictDeny, "deny:open_urls-value"},
		{"odd setting", Policy{Trust: TrustFallible, OpenURLs: "yes"}, r, VerdictDeny, "deny:open_urls-value"},
	})
}

func egressPolicy(t testing.TB, trust Trust, allow, deny string) Policy {
	d, err := ParseAllowOf("egress_deny", deny)
	if err != nil {
		t.Fatal(err)
	}
	return Policy{Trust: trust, Egress: &Egress{Ports: mustPorts(t, "22 443"), Allow: mustAllow(t, allow), Deny: d}}
}

func egressReq(host string, port int, as ...string) Request {
	r := Request{Kind: KindEgress, Host: host, Port: port, Addrs: addrs(as...)}
	if _, err := netip.ParseAddr(host); err == nil {
		r.Literal = true
	}
	return r
}

func TestDecideEgress(t *testing.T) {
	pub, priv := "140.82.112.3", "10.0.0.7"
	fb := func(allow, deny string) Policy { return egressPolicy(t, TrustFallible, allow, deny) }
	hb := func(allow, deny string) Policy { return egressPolicy(t, TrustHostile, allow, deny) }
	withGrant := func(p Policy, key string) Policy {
		p.Grants = []Grant{live(KindEgress, key, "g3")}
		return p
	}
	withAddrGrant := func(p Policy, key string, as ...string) Policy {
		g := live(KindEgress, key, "g4")
		g.Addrs = addrs(as...)
		p.Grants = []Grant{g}
		return p
	}
	expiredAddrGrant := func(p Policy, key string, as ...string) Policy {
		p.Grants = []Grant{{ID: "g4", Kind: KindEgress, Key: key, Expires: t0, Addrs: addrs(as...)}}
		return p
	}
	withGrants := func(p Policy, gs ...Grant) Policy {
		p.Grants = gs
		return p
	}
	ownReq := egressReq("me.example", 443, "203.0.113.9")
	ownReq.Own = addrs("::ffff:203.0.113.9")
	zoned := egressReq("x.example", 443, pub)
	zoned.Addrs = []netip.Addr{netip.MustParseAddr("fe80::1%en0")}
	mismatch := egressReq("10.0.0.7", 443, pub)
	runCases(t, []decideCase{
		{"no proxy", Policy{Trust: TrustFallible}, egressReq("x.example", 443, pub), VerdictDeny, "deny:egress-off"},
		{"bad name", fb("", ""), egressReq("X.example", 443, pub), VerdictDeny, "deny:egress-bad"},
		{"name with control", fb("", ""), egressReq("x\x1b.example", 443, pub), VerdictDeny, "deny:egress-bad"},
		{"literal not its address", fb("", ""), mismatch, VerdictDeny, "deny:egress-bad"},
		{"literal flag on a name", fb("", ""), Request{Kind: KindEgress, Host: "x.example", Literal: true, Port: 443, Addrs: addrs(pub)}, VerdictDeny, "deny:egress-bad"},
		{"literal with a zone", fb("", ""), Request{Kind: KindEgress, Host: "fe80::1%en0", Literal: true, Port: 443, Addrs: []netip.Addr{netip.MustParseAddr("fe80::1%en0")}}, VerdictDeny, "deny:egress-bad"},
		{"literal not canonical", fb("", ""), Request{Kind: KindEgress, Host: "::ffff:140.82.112.3", Literal: true, Port: 443, Addrs: addrs(pub)}, VerdictDeny, "deny:egress-bad"},
		{"literal not canonical, v6", fb("", ""), Request{Kind: KindEgress, Host: "2606:4700:0::1", Literal: true, Port: 443, Addrs: addrs("2606:4700::1")}, VerdictDeny, "deny:egress-bad"},
		{"literal with more addresses", fb("", ""), Request{Kind: KindEgress, Host: pub, Literal: true, Port: 443, Addrs: addrs(pub, priv)}, VerdictDeny, "deny:egress-bad"},
		{"no addresses", fb("", ""), egressReq("x.example", 443), VerdictDeny, "deny:egress-bad"},
		{"zoned address", fb("x.example", ""), zoned, VerdictDeny, "deny:egress-bad"},
		{"invalid address", fb("", ""), Request{Kind: KindEgress, Host: "x.example", Port: 443, Addrs: []netip.Addr{{}}}, VerdictDeny, "deny:egress-bad"},
		{"port", fb("x.example", ""), egressReq("x.example", 25, pub), VerdictDeny, "deny:egress_ports"},
		{"deny name over allow name", withGrant(fb("x.example", "x.example"), "egress:x.example"), egressReq("x.example", 443, pub), VerdictDeny, "deny:egress_deny"},
		{"deny pattern", fb("", "*.example"), egressReq("a.x.example", 443, pub), VerdictDeny, "deny:egress_deny"},
		{"deny pattern not the bare name", fb("", "*.x.example"), egressReq("x.example", 443, pub), VerdictAllow, "default:fallible-public"},
		{"deny address", fb("10.0.0.0/8", "140.82.0.0/16"), egressReq("x.example", 443, priv, pub), VerdictDeny, "deny:egress_deny"},
		{"deny embedded address", fb("", "8.8.8.8"), egressReq("x.example", 443, "64:ff9b::808:808"), VerdictDeny, "deny:egress_deny"},
		{"deny mapped address", fb("", "8.8.8.8"), egressReq("x.example", 443, "::ffff:8.8.8.8"), VerdictDeny, "deny:egress_deny"},
		{"deny name not a literal", fb("", "*.0.7"), egressReq("10.0.0.7", 443, priv), VerdictAsk, "ask:private"},
		{"loopback by name", withGrant(fb("*.nip.io", ""), "egress:x.nip.io"), egressReq("x.nip.io", 443, "127.0.0.1"), VerdictDeny, "deny:local-address"},
		{"loopback by cidr", fb("127.0.0.0/8", ""), egressReq("x.nip.io", 443, "127.0.0.1"), VerdictAllow, "allow:egress_allow"},
		{"own address", fb("", ""), ownReq, VerdictDeny, "deny:local-address"},
		{"own address, hostile", hb("", ""), ownReq, VerdictDeny, "deny:local-address"},
		{"allow name, private", fb("x.example", ""), egressReq("x.example", 443, priv), VerdictAllow, "allow:egress_allow"},
		{"allow pattern, private", fb("*.example", ""), egressReq("x.example", 443, pub, priv), VerdictAllow, "allow:egress_allow"},
		{"allow name and address, private", fb("x.example 10.0.0.7", ""), egressReq("x.example", 443, pub, priv), VerdictAllow, "allow:egress_allow"},
		{"allow name, public", fb("x.example", ""), egressReq("x.example", 443, pub), VerdictAllow, "allow:egress_allow"},
		{"allow name, private, hostile", hb("x.example", ""), egressReq("x.example", 443, priv), VerdictAllow, "allow:egress_allow"},
		{"allow name, hostile", hb("x.example", ""), egressReq("x.example", 443, pub), VerdictAllow, "allow:egress_allow"},
		{"allow cidr all", hb("10.0.0.0/24 140.82.0.0/16", ""), egressReq("x.example", 443, priv, pub), VerdictAllow, "allow:egress_allow"},
		{"allow cidr not all, hostile", hb("10.0.0.0/24", ""), egressReq("x.example", 443, priv, pub), VerdictAsk, "ask:hostile"},
		{"allow cidr the private one", fb("10.0.0.0/24", ""), egressReq("x.example", 443, priv, pub), VerdictAllow, "default:fallible-public"},
		{"allow cidr another private one", fb("10.0.1.0/24", ""), egressReq("x.example", 443, priv, pub), VerdictAsk, "ask:private"},
		{"allowed loopback by nat64", hb("127.0.0.1", ""), egressReq("x.example", 443, "64:ff9b::7f00:1"), VerdictAllow, "allow:egress_allow"},
		{"allow pattern not a literal", hb("*.0.7", ""), egressReq("10.0.0.7", 443, priv), VerdictAsk, "ask:private"},
		{"grant, public", withGrant(hb("", ""), "egress:x.example"), egressReq("x.example", 443, pub), VerdictAllow, "grant:g3"},
		{"grant by name, private", withGrant(hb("", ""), "egress:x.example"), egressReq("x.example", 443, priv), VerdictAsk, "ask:private"},
		{"grant by name, private, fallible", withGrant(fb("", ""), "egress:x.example"), egressReq("x.example", 443, pub, priv), VerdictAsk, "ask:private"},
		{"grant with the address", withAddrGrant(hb("", ""), "egress:x.example", priv), egressReq("x.example", 443, priv), VerdictAllow, "grant:g4"},
		{"grant with the address, mapped", withAddrGrant(hb("", ""), "egress:x.example", "::ffff:"+priv), egressReq("x.example", 443, pub, priv), VerdictAllow, "grant:g4"},
		{"grant with another address", withAddrGrant(hb("", ""), "egress:x.example", "10.0.0.8"), egressReq("x.example", 443, priv), VerdictAsk, "ask:private"},
		{"grant with one of two addresses", withAddrGrant(fb("", ""), "egress:x.example", priv), egressReq("x.example", 443, priv, "192.168.1.1"), VerdictAsk, "ask:private"},
		{"grant with the address, other host", withAddrGrant(fb("", ""), "egress:y.example", priv), egressReq("x.example", 443, priv), VerdictAsk, "ask:private"},
		{"expired grant with the address", expiredAddrGrant(fb("", ""), "egress:x.example", priv), egressReq("x.example", 443, priv), VerdictAsk, "ask:private"},
		{"grant with the address, then name only", withGrants(fb("", ""), Grant{ID: "g5", Kind: KindEgress, Key: "egress:x.example", Expires: t0.Add(time.Hour)}, Grant{ID: "g6", Kind: KindEgress, Key: "egress:x.example", Expires: t0.Add(time.Hour), Addrs: addrs(priv)}), egressReq("x.example", 443, priv), VerdictAllow, "grant:g6"},
		{"grant other host", withGrant(hb("", ""), "egress:a.x.example"), egressReq("x.example", 443, pub), VerdictAsk, "ask:hostile"},
		{"grant parent host", withGrant(hb("", ""), "egress:example"), egressReq("x.example", 443, pub), VerdictAsk, "ask:hostile"},
		{"private", fb("", ""), egressReq("x.example", 443, pub, priv), VerdictAsk, "ask:private"},
		{"hostile public", hb("", ""), egressReq("x.example", 443, pub), VerdictAsk, "ask:hostile"},
		{"odd trust public", egressPolicy(t, "", "", ""), egressReq("x.example", 443, pub), VerdictAsk, "ask:hostile"},
		{"fallible public", fb("", ""), egressReq("x.example", 443, pub), VerdictAllow, "default:fallible-public"},
		{"fallible public literal", fb("", ""), egressReq("2606:4700::1", 22, "2606:4700::1"), VerdictAllow, "default:fallible-public"},
	})
}

func TestParseAllowList(t *testing.T) {
	a, err := ParseAllowList("egress_deny", []string{"x.example", "*.y.example", "10.0.0.0/8"})
	if err != nil || !a.Name("x.example") || !a.Name("a.y.example") || !a.Addr(netip.MustParseAddr("10.1.1.1")) {
		t.Fatalf("%+v %v", a, err)
	}
	for _, bad := range [][]string{{"a b"}, {"a,b"}, {""}, {"none"}, {"*"}, {"a\tb"}} {
		if _, err := ParseAllowList("egress_deny", bad); err == nil || !strings.HasPrefix(err.Error(), "egress_deny: ") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := ParseAllowOf("egress_deny", "a..b"); err == nil || !strings.HasPrefix(err.Error(), "egress_deny: ") {
		t.Errorf("error does not name egress_deny: %v", err)
	}
	if a, err := ParseAllowList("egress_deny", nil); err != nil || a.Name("x") {
		t.Errorf("empty: %v", err)
	}
}

func TestSubstitute(t *testing.T) {
	a := Alias{
		Command: []string{"/bin/tool", "{mode}", "--{mode}", "{mode}x", "{other}", "{level}"},
		Params:  map[string][]string{"mode": {"fast", "slow"}, "level": {"1", "2"}},
	}
	got, err := Substitute(a, map[string]string{"mode": "slow"})
	if want := []string{"/bin/tool", "slow", "--{mode}", "{mode}x", "{other}", "1"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("Substitute = %q, %v; want %q", got, err, want)
	}
	if a.Command[1] != "{mode}" {
		t.Error("Substitute changed the alias")
	}
	for _, args := range []map[string]string{{"other": "x"}, {"mode": "medium"}, {"mode": "fast;rm"}} {
		if _, err := Substitute(a, args); err == nil {
			t.Errorf("Substitute accepted %v", args)
		}
	}
	if _, err := Substitute(Alias{}, nil); err == nil {
		t.Error("no command accepted")
	}
	if _, err := Substitute(Alias{Command: []string{"/x", "{p}"}, Params: map[string][]string{"p": nil}}, nil); err == nil {
		t.Error("a parameter with no value substituted")
	}
}

// The identity check: a command, cwd or argument whose identity, or that
// of a directory above it, is a writable path's is that path, whatever it
// is called; and a caller that found no identities where the policy has
// some is refused, since it did not look.
func TestAliasIdentities(t *testing.T) {
	w := FileID{Dev: 7, Ino: 70}
	other := FileID{Dev: 7, Ino: 71}
	base := func(trust Trust) (Policy, Request) {
		p := aliasPolicy(trust, AliasAllow)
		p.WritableIDs = []FileID{w}
		r := aliasReq()
		r.Cwd, r.ArgPaths = "/opt/work", []string{"/opt/arg"}
		p.Writable = []string{"/Users/me/dev/proj"}
		a := p.Aliases["build"]
		a.Command = []string{"/usr/bin/make", "{target}", "-C", "/opt/arg"}
		a.Cwd = "/opt/work"
		p.Aliases["build"] = a
		r.CommandIDs, r.CwdIDs, r.ArgIDs = []FileID{other}, []FileID{other}, []FileID{other}
		return p, r
	}
	for _, c := range []struct {
		name  string
		trust Trust
		f     func(*Request)
		want  Verdict
		rule  string
	}{
		{"none writable", TrustFallible, func(*Request) {}, VerdictAllow, "allow:alias"},
		{"command by identity", TrustFallible, func(r *Request) { r.CommandIDs = append(r.CommandIDs, w) }, VerdictDeny, "deny:alias-command-writable"},
		{"command identities missing", TrustFallible, func(r *Request) { r.CommandIDs = nil }, VerdictDeny, "deny:alias-unresolved"},
		{"cwd by identity, fallible", TrustFallible, func(r *Request) { r.CwdIDs = []FileID{w} }, VerdictAllow, "allow:alias"},
		{"cwd by identity, hostile", TrustHostile, func(r *Request) { r.CwdIDs = []FileID{w} }, VerdictDeny, "deny:hostile-alias-writable"},
		{"argument by identity, hostile", TrustHostile, func(r *Request) { r.ArgIDs = []FileID{other, w} }, VerdictDeny, "deny:hostile-alias-writable"},
		{"cwd identities missing", TrustHostile, func(r *Request) { r.CwdIDs = nil }, VerdictDeny, "deny:alias-unresolved"},
		{"argument identities missing", TrustHostile, func(r *Request) { r.ArgIDs = nil }, VerdictDeny, "deny:alias-unresolved"},
		{"none writable, hostile", TrustHostile, func(*Request) {}, VerdictAllow, "allow:alias"},
	} {
		p, r := base(c.trust)
		c.f(&r)
		d := Decide(p, r, t0)
		if d.Verdict != c.want || d.Rule != c.rule {
			t.Errorf("%s: %v %s (%s), want %v %s", c.name, d.Verdict, d.Rule, d.Reason, c.want, c.rule)
		}
	}
	// With no identities in the policy, a request without any is judged by
	// its paths alone.
	p, r := base(TrustFallible)
	p.WritableIDs, r.CommandIDs = nil, nil
	if d := Decide(p, r, t0); d.Verdict != VerdictAllow {
		t.Errorf("no identities either side: %v %s", d.Verdict, d.Reason)
	}
}

func TestIDOf(t *testing.T) {
	if _, ok := IDOf(nil); ok {
		t.Error("an identity for nothing")
	}
	a, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ia, ok1 := IDOf(a)
	ib, ok2 := IDOf(b)
	if !ok1 || !ok2 || ia == ib {
		t.Errorf("two directories: %v %v, %v %v", ia, ok1, ib, ok2)
	}
}
