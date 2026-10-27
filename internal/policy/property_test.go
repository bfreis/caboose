package policy

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
)

// The property tests draw policies and requests from pools chosen to
// collide: names that are prefixes or suffixes of one another, paths
// that differ only in case, addresses in every range Decide treats apart
// (in IPv4-mapped, NAT64, 6to4 and IPv4-translated forms too), and grants
// that are live, expired, never set, of another kind or of a near key.

const (
	propSeed  = 20261018
	propCases = 20000
)

var (
	trustPool      = []Trust{TrustFallible, TrustFallible, TrustFallible, TrustHostile, TrustHostile, "", "Fallible", "trusted"}
	kindPool       = []Kind{KindAlias, KindAlias, KindEgress, KindEgress, KindEgress, KindOpenURL, KindOpenURL, KindHostExec, KindCheckpoint, KindPort, KindPort, KindPort, "", "ALIAS", "exec"}
	checkpointPool = []string{CheckpointAllow, CheckpointAllow, CheckpointOff, "", "Allow", "ask"}
	aliasNames     = []string{"foo", "foobar", "fo", "build", ""}
	openURLsPool   = []string{OpenAsk, OpenAsk, OpenAllow, OpenOff, "", "yes"}
	policyPool     = []string{"", AliasAllow, AliasAsk, AliasAsk, AliasDeny, "Allow"}
	portPool       = []int{0, 22, 80, 1023, 1024, 3000, 4321, 7171, 7172, 65535, 70000}
	forwardPool    = []string{"3000-3999", "80 4321", "1-65535", "1024-65535", "22 80 3000", "none", "7000-7999", "4321"}
	originPool     = []string{"https://docs.example", "https://docs.example:8443", "http://docs.example", "https://evil.example"}
	hostPool       = []string{"a.example", "b.a.example", "example", "x.nip.io", "evil.example", "A.example", "a..b"}
	paramValues    = []string{"all", "test", "x", "/Users/me/dev/proj/run.sh", "/opt/tool"}
	writablePool   = []string{"/Users/me/dev/proj", "/users/ME/dev/Proj", "/Users/me/dev/other/out", "/opt", "/", "", "rel/dir", "/Users/me/dev/proj/"}
	pathPool       = []string{
		"/usr/bin/make", "/opt/tool/bin/x", "/Users/me/dev/proj/bin/run", "/USERS/me/DEV/proj/run",
		"/Users/me/dev/project/run", "/Users/me/dev/other/out", "/Users/me/dev/other/outer/x",
		"/opt", "/", "usr/bin/make", "/usr/bin/../bin/make", "",
	}
	addrPool = []string{
		"8.8.8.8", "140.82.112.3", "2606:4700::1111", // public
		"10.0.0.7", "192.168.64.2", "fd00::1", "100.64.0.1", "172.16.0.1", // private
		"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "::", // local
		"203.0.113.9", "2001:db8:1::5", // own, when Own says so
		"::ffff:10.0.0.1", "::ffff:127.0.0.1", "::ffff:8.8.8.8", "::ffff:203.0.113.9", // IPv4-mapped
		"64:ff9b::7f00:1", "64:ff9b::808:808", "64:ff9b::cb00:7109", "64:ff9b:1::a00:1", // NAT64
		"2002:a00:1::", "2002:808:808::", "2002:7f00:1::", // 6to4
		"::ffff:0:7f00:1", "::ffff:0:808:808", // IPv4-translated
	}
	ownPool       = []string{"203.0.113.9", "2001:db8:1::5", "::ffff:10.0.0.7", "127.0.0.1"}
	allowNamePool = []string{"a.example", "*.a.example", "*.example", "x.nip.io", "*.nip.io", "example", "*.0.7"}
	allowNetPool  = []string{
		"10.0.0.0/8", "10.0.0.7", "127.0.0.1", "127.0.0.0/8", "0.0.0.0/0", "::/0", "203.0.113.9",
		"8.8.8.8", "140.82.0.0/16", "fd00::/8", "2001:db8::/32", "2001:db8:1::5", "64:ff9b::/96",
		"fe80::/10", "169.254.169.254", "::ffff:10.0.0.0/104", "192.168.64.0/24",
	}
)

func pick[T any](rng *rand.Rand, s []T) T { return s[rng.IntN(len(s))] }

func subset[T any](rng *rand.Rand, s []T, maxN int) []T {
	var out []T
	for range rng.IntN(maxN + 1) {
		out = append(out, pick(rng, s))
	}
	return out
}

type gen struct {
	rng *rand.Rand
	ids int
}

func (g *gen) id() string {
	g.ids++
	return fmt.Sprintf("g%d", g.ids)
}

func (g *gen) alias(name string) Alias {
	rng := g.rng
	params := map[string][]string{}
	for range rng.IntN(3) {
		params[pick(rng, []string{"target", "mode", "file"})] = subset(rng, paramValues, 3)
	}
	cmd := []string{pick(rng, pathPool)}
	for range rng.IntN(4) {
		cmd = append(cmd, pick(rng, []string{"{target}", "{mode}", "{file}", "-v", "/Users/me/dev/proj", "/etc/x", "{other}"}))
	}
	cwd := ""
	if rng.IntN(2) == 0 {
		cwd = pick(rng, pathPool)
	}
	return Alias{Name: name, Command: cmd, Cwd: cwd, Params: params, Policy: pick(rng, policyPool)}
}

func (g *gen) allowList(maxN int) Allow {
	var entries []string
	entries = append(entries, subset(g.rng, allowNamePool, maxN)...)
	entries = append(entries, subset(g.rng, allowNetPool, maxN)...)
	a, err := ParseAllowList("egress_allow", entries)
	if err != nil {
		panic(err)
	}
	return a
}

func (g *gen) grant(kind Kind, key string) Grant {
	rng := g.rng
	gr := Grant{ID: g.id(), Kind: kind, Key: key, Once: rng.IntN(2) == 0}
	if rng.IntN(3) == 0 {
		for _, s := range subset(rng, addrPool, 3) {
			gr.Addrs = append(gr.Addrs, netip.MustParseAddr(s))
		}
	}
	switch rng.IntN(4) {
	case 0:
		gr.Expires = t0.Add(-time.Duration(rng.IntN(1000)) * time.Second) // expired, or expiring now
	case 1:
		// never set
	default:
		gr.Expires = t0.Add(time.Duration(1+rng.IntN(1000)) * time.Second)
	}
	return gr
}

// grantKeyPool is keys near what the generated requests carry.
func grantKeyPool() []string {
	var keys []string
	for _, n := range aliasNames {
		keys = append(keys, "alias:"+n)
	}
	for _, h := range append(hostPool, addrPool...) {
		keys = append(keys, "egress:"+h)
	}
	for _, o := range originPool {
		keys = append(keys, "open_url:"+o)
	}
	for _, n := range portPool {
		keys = append(keys, fmt.Sprintf("port:%d", n))
	}
	return append(keys, "", "alias:", "egress:a.example.", "open_url:https://docs.example/")
}

var keys = grantKeyPool()

func (g *gen) policy() Policy {
	rng := g.rng
	p := Policy{
		Trust:              pick(rng, trustPool),
		HostExec:           rng.IntN(2) == 0,
		OpenURLs:           pick(rng, openURLsPool),
		OpenURLAllow:       subset(rng, originPool, 2),
		CheckpointRequests: pick(rng, checkpointPool),
		Aliases:            map[string]Alias{},
		Writable:           subset(rng, writablePool, 3),
		WritableIDs:        subset(rng, idPool, 2),
	}
	if fp, err := config.ParsePorts(pick(rng, forwardPool)); err == nil {
		p.ForwardPorts = fp
	}
	for _, n := range aliasNames {
		if n != "" && rng.IntN(3) > 0 {
			p.Aliases[n] = g.alias(n)
		}
	}
	if rng.IntN(8) > 0 {
		ports, err := config.ParsePortsOf("egress_ports", pick(rng, []string{"22", "443", "80 443", "22 443", "443 7171", "7000-7999"}))
		if err != nil {
			panic(err)
		}
		p.Egress = &Egress{Ports: ports, Allow: g.allowList(2), Deny: g.allowList(1)}
	}
	for range rng.IntN(4) {
		p.Grants = append(p.Grants, g.grant(pick(rng, kindPool), pick(rng, keys)))
	}
	return p
}

func (g *gen) request(p Policy) Request {
	rng := g.rng
	r := Request{Kind: pick(rng, kindPool)}
	// Every kind's fields are filled now and then, to show they are
	// ignored where they do not belong.
	if r.Kind == KindAlias || rng.IntN(4) == 0 {
		r.Alias = pick(rng, aliasNames)
		a := p.Aliases[r.Alias]
		if rng.IntN(5) > 0 {
			r.Args = map[string]string{}
			for k, vs := range a.Params {
				if len(vs) > 0 && rng.IntN(2) == 0 {
					r.Args[k] = pick(rng, vs)
				}
			}
			if rng.IntN(10) == 0 {
				r.Args[pick(rng, []string{"target", "other"})] = pick(rng, paramValues)
			}
		}
		r.Command = pick(rng, pathPool)
		r.CommandIDs = subset(rng, idPool, 3)
		r.CwdIDs = subset(rng, idPool, 2)
		r.ArgIDs = subset(rng, idPool, 2)
		if a.Cwd != "" || rng.IntN(5) == 0 {
			r.Cwd = pick(rng, pathPool)
		}
		if argv, err := Substitute(a, r.Args); err == nil && rng.IntN(5) > 0 {
			for _, e := range argv[1:] {
				if strings.HasPrefix(e, "/") {
					r.ArgPaths = append(r.ArgPaths, pick(rng, append(pathPool, e)))
				}
			}
		} else {
			r.ArgPaths = subset(rng, pathPool, 2)
		}
	}
	if r.Kind == KindEgress || rng.IntN(4) == 0 {
		r.Port = pick(rng, []int{22, 443, 80, 25, 7171, 7172})
		r.Own = subset(rng, addrsOf(ownPool), 2)
		if rng.IntN(3) == 0 {
			a := netip.MustParseAddr(pick(rng, addrPool))
			r.Host, r.Literal, r.Addrs = a.String(), true, []netip.Addr{a}
			if rng.IntN(10) == 0 {
				r.Addrs = append(r.Addrs, netip.MustParseAddr(pick(rng, addrPool)))
			}
		} else {
			r.Host = pick(rng, hostPool)
			r.Addrs = subset(rng, addrsOf(addrPool), 3)
			if len(r.Addrs) == 0 && rng.IntN(3) > 0 {
				r.Addrs = addrsOf([]string{pick(rng, addrPool)})
			}
		}
		if rng.IntN(30) == 0 && len(r.Addrs) > 0 {
			r.Addrs[0] = r.Addrs[0].WithZone("en0")
		}
	}
	if r.Kind == KindPort {
		r.Port = pick(rng, portPool)
	}
	if r.Kind == KindPort || rng.IntN(4) == 0 {
		r.HostChecked, r.HostListens = rng.IntN(3) > 0, rng.IntN(3) == 0
	}
	if r.Kind == KindOpenURL || rng.IntN(4) == 0 {
		r.Origin = pick(rng, originPool)
		r.URL = r.Origin + pick(rng, []string{"/", "/a?b", "", "#x"})
		if rng.IntN(10) == 0 {
			r.URL = pick(rng, originPool) + ".evil/"
		}
		if rng.IntN(20) == 0 {
			r.Origin = ""
		}
	}
	return r
}

// idPool are the file identities the generators draw from.
var idPool = []FileID{{1, 1}, {1, 2}, {2, 1}, {9, 9}}

// An alias whose command's identity chain meets a writable identity is
// never allowed; under hostile, neither is one whose cwd's or an
// argument's does.
func TestPropertyAliasIdentityWritable(t *testing.T) {
	forCases(t, "alias identity", func(g *gen, p Policy, r Request) string {
		if r.Kind != KindAlias {
			return ""
		}
		d := Decide(p, r, t0)
		if d.Verdict != VerdictAllow {
			return ""
		}
		meets := func(ids []FileID) bool {
			for _, id := range ids {
				if slices.Contains(p.WritableIDs, id) {
					return true
				}
			}
			return false
		}
		if meets(r.CommandIDs) {
			return fmt.Sprintf("allowed a command whose identities %v meet writable %v by %s", r.CommandIDs, p.WritableIDs, d.Rule)
		}
		if p.Trust != TrustFallible && (meets(r.CwdIDs) || meets(r.ArgIDs)) {
			return fmt.Sprintf("under %q allowed a cwd or argument whose identities meet writable %v by %s", p.Trust, p.WritableIDs, d.Rule)
		}
		if len(p.WritableIDs) > 0 && len(r.CommandIDs) == 0 {
			return fmt.Sprintf("allowed a command with no identities by %s", d.Rule)
		}
		return ""
	})
}

func addrsOf(ss []string) []netip.Addr {
	var out []netip.Addr
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

// clone copies what a widening changes, so the original stays as drawn.
func clone(p Policy) Policy {
	q := p
	q.OpenURLAllow = slices.Clone(p.OpenURLAllow)
	q.Grants = slices.Clone(p.Grants)
	q.Aliases = map[string]Alias{}
	for k, v := range p.Aliases {
		q.Aliases[k] = v
	}
	if p.Egress != nil {
		e := *p.Egress
		q.Egress = &e
	}
	return q
}

func forCases(t *testing.T, name string, f func(g *gen, p Policy, r Request) string) {
	t.Helper()
	g := &gen{rng: rand.New(rand.NewPCG(propSeed, 0))}
	for i := range propCases {
		p := g.policy()
		r := g.request(p)
		if g.rng.IntN(3) == 0 {
			// A grant for the request's own key, live or not.
			p.Grants = append(p.Grants, g.grant(r.Kind, GrantKey(r)))
		}
		if msg := f(g, p, r); msg != "" {
			t.Fatalf("%s: seed %d, case %d: %s\npolicy %+v\negress %+v\nrequest %+v", name, propSeed, i, msg, p, p.Egress, r)
		}
	}
}

// Deny always wins: when Decide denies a request, no widening of the
// policy turns that into Allow. A widening adds grants of every kind
// (a live one for the request's own key among them), names to
// egress_allow (the request's host among them), addresses and CIDRs to
// egress_allow (except when the denial is of a local address, which
// only such an entry is meant to lift), origins to open_urls_allow (the
// request's own among them), turns open_urls = "ask" into "allow" and
// policy = "ask" into "allow" on every alias, and, unless the denial is
// one of trust = hostile's own, sets any trust level. Settings that
// refuse ("off", "deny", or a value Decide does not know) are left as
// they are: changing those is not adding an allowance.
func TestPropertyDenyWins(t *testing.T) {
	forCases(t, "deny wins", func(g *gen, p Policy, r Request) string {
		d := Decide(p, r, t0)
		if d.Verdict != VerdictDeny {
			return ""
		}
		rng := g.rng
		q := clone(p)
		for _, k := range []Kind{r.Kind, KindAlias, KindEgress, KindOpenURL, KindHostExec} {
			q.Grants = append(q.Grants, Grant{ID: g.id(), Kind: k, Key: GrantKey(r), Expires: t0.Add(time.Hour)})
		}
		for range rng.IntN(4) {
			q.Grants = append(q.Grants, Grant{ID: g.id(), Kind: pick(rng, kindPool), Key: pick(rng, keys), Expires: t0.Add(time.Hour)})
		}
		if d.Rule != "deny:port-privileged" {
			if fp, err := config.ParsePorts("1-65535"); err == nil {
				q.ForwardPorts = fp
			}
		}
		if q.Egress != nil {
			entries := append(subset(rng, allowNamePool, 3), "a.example", "*.example")
			if validName(r.Host) && !r.Literal {
				entries = append(entries, r.Host, "*."+r.Host)
			}
			if d.Rule != "deny:local-address" {
				entries = append(entries, subset(rng, allowNetPool, 4)...)
				entries = append(entries, "0.0.0.0/0", "::/0")
			}
			a, err := ParseAllowList("egress_allow", entries)
			if err != nil {
				return err.Error()
			}
			q.Egress.Allow = merge(q.Egress.Allow, a)
		}
		q.OpenURLAllow = append(q.OpenURLAllow, originPool...)
		if r.Origin != "" {
			q.OpenURLAllow = append(q.OpenURLAllow, r.Origin)
		}
		if q.OpenURLs == OpenAsk {
			q.OpenURLs = OpenAllow
		}
		for k, a := range q.Aliases {
			if a.Policy == AliasAsk {
				a.Policy = AliasAllow
				q.Aliases[k] = a
			}
		}
		if !strings.HasPrefix(d.Rule, "deny:hostile-") {
			q.Trust = pick(rng, trustPool)
		}
		if d2 := Decide(q, r, t0); d2.Verdict == VerdictAllow {
			return fmt.Sprintf("denied by %s (%s), allowed once widened by %s (%s)", d.Rule, d.Reason, d2.Rule, d2.Reason)
		}
		return ""
	})
}

// The host's web UI ports are never reached on a local address (loopback,
// unspecified, link-local, this machine's own, in any form that stands
// for one): whatever egress_allow (127.0.0.1 and 0.0.0.0/0 among it),
// egress_ports, a live grant holding the very addresses, or the trust
// level say.
func TestPropertyUIPort(t *testing.T) {
	n := 0
	forCases(t, "ui port", func(g *gen, p Policy, r Request) string {
		if r.Kind != KindEgress || p.Egress == nil {
			return ""
		}
		r.Port = pick(g.rng, []int{7171, 7172})
		own := make([]netip.Addr, 0, len(r.Own))
		for _, a := range r.Own {
			own = append(own, a.Unmap().WithZone(""))
		}
		local := false
		for _, a := range r.Addrs {
			if _, _, ok := localRange(a, own); ok {
				local = true
			}
		}
		q := clone(p)
		ports, err := config.ParsePortsOf("egress_ports", "1-65535")
		if err != nil {
			return err.Error()
		}
		entries := []string{"127.0.0.1", "127.0.0.0/8", "0.0.0.0/0", "::/0", "::1", "*.example", "a.example"}
		if validName(r.Host) && !r.Literal {
			entries = append(entries, r.Host)
		}
		a, err := ParseAllowList("egress_allow", entries)
		if err != nil {
			return err.Error()
		}
		q.Egress.Ports, q.Egress.Allow, q.Egress.Deny = ports, merge(q.Egress.Allow, a), Allow{}
		q.Grants = append(q.Grants, Grant{ID: g.id(), Kind: KindEgress, Key: GrantKey(r), Addrs: r.Addrs, Expires: t0.Add(time.Hour)})
		q.Trust = pick(g.rng, trustPool)
		for _, pol := range []Policy{p, q} {
			d := Decide(pol, r, t0)
			if local && d.Verdict != VerdictDeny {
				return fmt.Sprintf("a local address on port %d: %s by %s (%s)", r.Port, d.Verdict, d.Rule, d.Reason)
			}
		}
		if local {
			n++
		}
		return ""
	})
	if n < 100 {
		t.Fatalf("only %d cases reached a local address on a UI port", n)
	}
}

func merge(a, b Allow) Allow {
	return Allow{
		names:    append(slices.Clone(a.names), b.names...),
		suffixes: append(slices.Clone(a.suffixes), b.suffixes...),
		nets:     append(slices.Clone(a.nets), b.nets...),
	}
}

// Under any trust but fallible, host exec is never allowed, a URL never
// opens without asking, and a connection is allowed only when
// egress_allow names its host or every address, or a live grant names
// its host.
func TestPropertyHostile(t *testing.T) {
	forCases(t, "hostile", func(g *gen, p Policy, r Request) string {
		if p.Trust == TrustFallible {
			p.Trust = pick(g.rng, []Trust{TrustHostile, "", "x"})
		}
		d := Decide(p, r, t0)
		if d.Verdict != VerdictAllow {
			return ""
		}
		switch r.Kind {
		case KindHostExec, KindOpenURL:
			return fmt.Sprintf("allowed under trust %q by %s", p.Trust, d.Rule)
		case KindEgress:
			if !r.Literal && p.Egress.Allow.Name(r.Host) {
				return ""
			}
			all := len(r.Addrs) > 0
			for _, a := range r.Addrs {
				named := p.Egress.Allow.Addr(a)
				for _, v4 := range embeddedV4(a.Unmap()) {
					named = named || p.Egress.Allow.Addr(v4)
				}
				all = all && named
			}
			if all {
				return ""
			}
			for _, gr := range p.Grants {
				if gr.Kind == KindEgress && gr.Key == "egress:"+r.Host && t0.Before(gr.Expires) {
					return ""
				}
			}
			return fmt.Sprintf("egress allowed under trust %q by %s, which nothing names", p.Trust, d.Rule)
		}
		return ""
	})
}

// Expired grants, and grants with no expiry, change nothing; and a
// decision names a grant only when it is live and of the request's own
// kind and key.
func TestPropertyGrants(t *testing.T) {
	forCases(t, "grants", func(g *gen, p Policy, r Request) string {
		d := Decide(p, r, t0)
		q := clone(p)
		for _, k := range []Kind{r.Kind, pick(g.rng, kindPool)} {
			q.Grants = append(q.Grants,
				Grant{ID: g.id(), Kind: k, Key: GrantKey(r), Expires: t0},
				Grant{ID: g.id(), Kind: k, Key: GrantKey(r), Expires: t0.Add(-time.Nanosecond), Once: true},
				Grant{ID: g.id(), Kind: k, Key: GrantKey(r)},
			)
		}
		if d2 := Decide(q, r, t0); !reflect.DeepEqual(d, d2) {
			return fmt.Sprintf("expired grants changed %+v into %+v", d, d2)
		}
		if d.Grant == "" {
			if strings.HasPrefix(d.Rule, "grant:") {
				return "a grant rule names no grant"
			}
			return ""
		}
		if d.Verdict != VerdictAllow || d.Rule != "grant:"+d.Grant {
			return fmt.Sprintf("grant %s on %+v", d.Grant, d)
		}
		for _, gr := range p.Grants {
			if gr.ID == d.Grant {
				if gr.Kind != r.Kind || gr.Key == "" || gr.Key != GrantKey(r) || !t0.Before(gr.Expires) {
					return fmt.Sprintf("grant %+v allowed a request of key %q", gr, GrantKey(r))
				}
				return ""
			}
		}
		return "the decision names a grant the policy does not hold"
	})
}

// A grant for one key allows nothing else: a policy whose only path to
// Allow is one live grant allows exactly the requests of its key and
// kind.
func TestPropertyGrantKeyExact(t *testing.T) {
	forCases(t, "grant key", func(g *gen, p Policy, r Request) string {
		gr := Grant{ID: g.id(), Kind: pick(g.rng, kindPool), Key: pick(g.rng, keys), Expires: t0.Add(time.Hour)}
		p.Grants = []Grant{gr}
		d := Decide(p, r, t0)
		if d.Grant != "" && (gr.Key != GrantKey(r) || gr.Kind != r.Kind) {
			return fmt.Sprintf("grant %+v allowed key %q of kind %q", gr, GrantKey(r), r.Kind)
		}
		return ""
	})
}

// An alias whose command lies within a writable path, compared
// component by component and ignoring case, is never allowed.
func TestPropertyAliasCommandWritable(t *testing.T) {
	forCases(t, "alias command", func(g *gen, p Policy, r Request) string {
		if r.Kind != KindAlias {
			return ""
		}
		d := Decide(p, r, t0)
		if d.Verdict != VerdictAllow {
			return ""
		}
		cmd := strings.ToLower(r.Command)
		for _, w := range p.Writable {
			if w == "" {
				continue
			}
			lw := strings.TrimSuffix(strings.ToLower(w), "/")
			if !strings.HasPrefix(w, "/") || lw == "" || cmd == lw || strings.HasPrefix(cmd, lw+"/") {
				return fmt.Sprintf("allowed a command %s within writable %s by %s", r.Command, w, d.Rule)
			}
		}
		return ""
	})
}

// Unknown kinds are denied, and Decide is deterministic.
func TestPropertyUnknownAndDeterministic(t *testing.T) {
	forCases(t, "unknown", func(g *gen, p Policy, r Request) string {
		d := Decide(p, r, t0)
		if d2 := Decide(p, r, t0); !reflect.DeepEqual(d, d2) {
			return fmt.Sprintf("two decisions: %+v and %+v", d, d2)
		}
		switch r.Kind {
		case KindAlias, KindEgress, KindOpenURL, KindHostExec, KindCheckpoint, KindPort:
		default:
			if d.Verdict != VerdictDeny || d.Rule != "deny:unknown-kind" {
				return fmt.Sprintf("kind %q decided %+v", r.Kind, d)
			}
		}
		if d.Reason == "" || d.Rule == "" {
			return fmt.Sprintf("no reason or rule: %+v", d)
		}
		return ""
	})
}

// A grant lets a private address through only when it holds that very
// address: a grant by name, made for one resolution, never reaches a
// private address the name later resolves to (DNS rebinding), at any
// trust. Only config.toml's egress_allow, which the user wrote, reaches
// one by name.
func TestPropertyGrantPrivateNeedsAddress(t *testing.T) {
	forCases(t, "private", func(g *gen, p Policy, r Request) string {
		if r.Kind != KindEgress || p.Egress == nil {
			return ""
		}
		if g.rng.IntN(2) == 0 {
			// A live grant by name, holding no address.
			p.Grants = append(p.Grants, Grant{ID: "gname", Kind: KindEgress, Key: GrantKey(r), Expires: t0.Add(time.Hour)})
		}
		d := Decide(p, r, t0)
		if d.Verdict != VerdictAllow || d.Grant == "" {
			return ""
		}
		own := make([]netip.Addr, 0, len(r.Own))
		for _, a := range r.Own {
			own = append(own, a.Unmap().WithZone(""))
		}
		var gr Grant
		for _, x := range p.Grants {
			if x.ID == d.Grant {
				gr = x
			}
		}
		for _, a := range r.Addrs {
			if !Denied(a) || allowedAddr(p.Egress.Allow, a, own) {
				continue
			}
			held := false
			for _, h := range gr.Addrs {
				held = held || h.Unmap() == a.Unmap().WithZone("")
			}
			if !held {
				return fmt.Sprintf("private address %s allowed by grant %s, which does not hold it", a, d.Grant)
			}
		}
		return ""
	})
}

// A checkpoint is allowed exactly when checkpoint_requests is "allow",
// whatever the trust and the grants; it never asks, and no grant allows
// one.
func TestPropertyCheckpoint(t *testing.T) {
	forCases(t, "checkpoint", func(g *gen, p Policy, r Request) string {
		if r.Kind != KindCheckpoint {
			return ""
		}
		d := Decide(p, r, t0)
		want := VerdictDeny
		if p.CheckpointRequests == CheckpointAllow {
			want = VerdictAllow
		}
		if d.Verdict != want || d.Grant != "" {
			return fmt.Sprintf("checkpoint_requests %q, trust %q: %+v", p.CheckpointRequests, p.Trust, d)
		}
		return ""
	})
}

// A port is never allowed while a program on the host listens there, while
// the host has not checked it, or when it is a web UI port; and a grant
// never forwards one below 1024, which only forward_ports does.
func TestPropertyPortRefusals(t *testing.T) {
	forCases(t, "port refusals", func(g *gen, p Policy, r Request) string {
		if r.Kind != KindPort {
			return ""
		}
		d := Decide(p, r, t0)
		if d.Verdict != VerdictAllow {
			return ""
		}
		switch {
		case r.HostListens:
			return fmt.Sprintf("port %d allowed by %s although the host listens there", r.Port, d.Rule)
		case !r.HostChecked:
			return fmt.Sprintf("port %d allowed by %s before the host checked it", r.Port, d.Rule)
		case r.Port == 7171 || r.Port == 7172:
			return fmt.Sprintf("web UI port %d allowed by %s", r.Port, d.Rule)
		case r.Port < 1 || r.Port > 65535:
			return fmt.Sprintf("port %d allowed by %s", r.Port, d.Rule)
		case r.Port < 1024 && strings.HasPrefix(d.Rule, "grant:"):
			return fmt.Sprintf("port %d allowed by %s, a grant", r.Port, d.Rule)
		}
		return ""
	})
}

// The trust does not enter into a port's decision.
func TestPropertyPortTrustIrrelevant(t *testing.T) {
	forCases(t, "port trust", func(g *gen, p Policy, r Request) string {
		if r.Kind != KindPort {
			return ""
		}
		want := Decide(p, r, t0)
		for _, tr := range trustPool {
			p.Trust = tr
			if got := Decide(p, r, t0); got.Verdict != want.Verdict || got.Rule != want.Rule {
				return fmt.Sprintf("trust %q decided %v by %s, not %v by %s", tr, got.Verdict, got.Rule, want.Verdict, want.Rule)
			}
		}
		return ""
	})
}
