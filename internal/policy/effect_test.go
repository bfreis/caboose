package policy

import (
	"strings"
	"testing"
	"unicode"
)

func TestEffect(t *testing.T) {
	p := aliasPolicy(TrustFallible, "")
	p.Aliases["fresh"] = Alias{Command: []string{"/bin/echo", "a b", ""}}
	own := egressReq("me.example", 443, "203.0.113.9", "10.0.0.1")
	own.Own = addrs("203.0.113.9")
	for _, c := range []struct {
		r    Request
		want string
	}{
		{aliasReq(), "run alias build on the host: /usr/bin/make test -C /Users/me/dev/proj in /Users/me/dev/proj"},
		{func() Request { r := aliasReq(); r.Command, r.Args = "/opt/make/bin/make", nil; return r }(),
			"run alias build on the host: /opt/make/bin/make all -C /Users/me/dev/proj in /Users/me/dev/proj"},
		{Request{Kind: KindAlias, Alias: "fresh", Command: "/bin/echo"}, `run alias fresh on the host: /bin/echo "a b" "" in a fresh empty directory`},
		{Request{Kind: KindAlias, Alias: "nope"}, "run alias nope on the host, which has no such alias"},
		{Request{Kind: KindAlias, Alias: "build", Args: map[string]string{"target": "x"}}, "run alias build on the host, which refuses its arguments"},
		{egressReq("x.example", 443, "140.82.112.3", "10.0.0.7"), "connect to x.example:443 (resolves to 140.82.112.3, 10.0.0.7; private)"},
		{egressReq("x.example", 22, "140.82.112.3"), "connect to x.example:22 (resolves to 140.82.112.3)"},
		{egressReq("10.0.0.7", 22, "10.0.0.7"), "connect to 10.0.0.7:22 (private)"},
		{egressReq("2606:4700::1", 443, "2606:4700::1"), "connect to [2606:4700::1]:443"},
		{own, "connect to me.example:443 (resolves to 203.0.113.9, 10.0.0.1; this machine; private)"},
		{Request{Kind: KindOpenURL, URL: "https://x.example/a"}, "open https://x.example/a in the browser"},
		{Request{Kind: KindHostExec}, "run a command on the host, through host_exec"},
		{Request{Kind: "x"}, "an unknown request, which the host refuses"},
	} {
		if got := Effect(p, c.r); got != c.want {
			t.Errorf("Effect(%+v)\n got %q\nwant %q", c.r, got, c.want)
		}
	}
}

// What the sandbox sent is shown printable and clipped, never as it came.
func TestEffectPrintable(t *testing.T) {
	evil := "x\x1b[31m\u202e\u2028\x00" + strings.Repeat("y", 5000)
	p := Policy{Aliases: map[string]Alias{evil: {Command: []string{"/bin/x", "{a}"}, Params: map[string][]string{"a": {evil}}}}}
	for _, r := range []Request{
		{Kind: KindAlias, Alias: evil, Args: map[string]string{"a": evil}, Command: "/bin/x" + evil, Cwd: "/" + evil},
		{Kind: KindAlias, Alias: evil},
		{Kind: KindEgress, Host: evil, Port: 1},
		{Kind: KindOpenURL, URL: "https://" + evil, Origin: "https://" + evil},
	} {
		check := func(what, s string) {
			for _, c := range s {
				if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) || c == '\u2028' {
					t.Errorf("%s of %v holds %U", what, r.Kind, c)
				}
			}
			if len(s) > 2000 {
				t.Errorf("%s of %v is %d bytes", what, r.Kind, len(s))
			}
		}
		check("Effect", Effect(p, r))
		for _, s := range []Scope{ScopeOnce, ScopeHours, ScopeAlways} {
			check("AnswerEffect", AnswerEffect(r, s, 2, "default", "default", "/c.toml"))
		}
		check("Decide", Decide(Policy{Trust: TrustFallible, Egress: &Egress{}, OpenURLs: "x" + evil}, r, t0).Reason)
	}
}

func TestAnswerEffect(t *testing.T) {
	al := Request{Kind: KindAlias, Alias: "build"}
	eg := Request{Kind: KindEgress, Host: "api.example"}
	ur := Request{Kind: KindOpenURL, URL: "https://docs.example/x", Origin: "https://docs.example"}
	pv := Request{Kind: KindEgress, Host: "git.corp.example", Addrs: addrs("140.82.112.3", "::ffff:10.0.0.7", "10.0.0.7", "192.168.1.2")}
	for _, c := range []struct {
		r     Request
		s     Scope
		hours int
		want  string
	}{
		{al, ScopeOnce, 0, "allows that once"},
		{eg, ScopeOnce, 0, "allows that once"},
		{ur, ScopeOnce, 0, "opens that URL now, and allows nothing more"},
		{al, ScopeHours, 8, "allows alias build in environment work for 8 hours"},
		{eg, ScopeHours, 1, "allows connections to api.example in environment work for 1 hour"},
		{ur, ScopeHours, 2, "allows opening URLs of https://docs.example in environment work for 2 hours"},
		{al, ScopeHours, 0, ""},
		{al, ScopeAlways, 0, `sets policy = "allow" in [alias.build] of /h/config.toml`},
		{eg, ScopeAlways, 0, `adds "api.example" to egress_allow in [vm.default] of /h/config.toml`},
		{ur, ScopeAlways, 0, `adds "https://docs.example" to open_urls_allow in [link] of /h/config.toml`},
		{pv, ScopeHours, 2, "allows connections to git.corp.example at 10.0.0.7, 192.168.1.2 in environment work for 2 hours"},
		{pv, ScopeAlways, 0, `adds "git.corp.example" to egress_allow in [vm.default] of /h/config.toml`},
		{Request{Kind: KindHostExec}, ScopeAlways, 0, ""},
		{Request{Kind: KindAlias}, ScopeOnce, 0, ""},
		{al, Scope(0), 0, ""},
	} {
		if got := AnswerEffect(c.r, c.s, c.hours, "work", "default", "/h/config.toml"); got != c.want {
			t.Errorf("AnswerEffect(%v, %d)\n got %q\nwant %q", c.r.Kind, c.s, got, c.want)
		}
	}
}

func TestNonPublicAddrs(t *testing.T) {
	r := Request{Kind: KindEgress, Host: "x.example", Addrs: addrs("8.8.8.8", "::ffff:10.0.0.7", "10.0.0.7", "64:ff9b::a00:1", "fe80::1%en0")}
	got := NonPublicAddrs(r)
	want := addrs("10.0.0.7", "64:ff9b::a00:1", "fe80::1")
	if len(got) != len(want) {
		t.Fatalf("NonPublicAddrs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NonPublicAddrs = %v, want %v", got, want)
		}
	}
	r.Kind = KindAlias
	if got := NonPublicAddrs(r); got != nil {
		t.Errorf("NonPublicAddrs of an alias = %v", got)
	}
}

func TestEgressRuleEffect(t *testing.T) {
	for _, c := range []struct {
		trust     Trust
		deny, add bool
		entry     string
		want      string
	}{
		{TrustFallible, false, true, "git.corp.example", "allows connections to git.corp.example from environment e at any address it resolves to, private ones included (public hosts connect anyway under trust = fallible)"},
		{TrustFallible, false, true, "10.0.0.0/8", "allows connections to addresses in 10.0.0.0/8 from environment e (public hosts connect anyway under trust = fallible)"},
		{TrustHostile, false, true, "*.corp.example", "allows connections to every name under corp.example from environment e without asking at any address it resolves to, private ones included"},
		{TrustHostile, false, true, "10.1.2.3", "allows connections to address 10.1.2.3 from environment e without asking"},
		{TrustFallible, false, false, "git.corp.example", "changes nothing for public hosts, which connect anyway under trust = fallible: connections to git.corp.example from environment e that reach a private or local address ask again"},
		{"", false, false, "git.corp.example", "connections to git.corp.example from environment e ask again (trust = hostile asks for every host egress_allow does not name)"},
		{TrustFallible, true, true, "ads.example", "denies connections to ads.example from environment e, whatever egress_allow or a grant says"},
		{TrustHostile, true, false, "ads.example", "stops denying connections to ads.example from environment e, which the other rules decide again"},
	} {
		if got := EgressRuleEffect(c.trust, c.deny, c.add, c.entry, "e"); got != c.want {
			t.Errorf("%+v:\n got %q\nwant %q", c, got, c.want)
		}
	}
}
