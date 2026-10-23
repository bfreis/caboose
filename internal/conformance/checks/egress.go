package checks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	c "github.com/bfreis/caboose/internal/conformance"
)

// The egress checks, E1-E8: under vm, every outbound connection the
// sandbox makes through the outbound proxy is decided by the host, and a
// hostile guest has no network of its own; under container and gVisor,
// egress is not restricted (a documented gap). The only thing any of
// them connects to is a listener the suite started, on 127.0.0.1 or on
// this machine's own LAN address (E4 also sends to the documentation
// address 192.0.2.1, which no packet reaches). Whether a connection
// arrived is the listener's count, on the host: the sandbox's own report
// can only turn a PASS into a FAIL or an ERROR, never the other way.

const (
	groupEgress = "egress"
	stepEgress  = "egress through the host"

	// egressSettle is how long a check waits, after its last attempt,
	// before it reads the listeners' counts for the last time: a
	// connection the host let through late still arrives.
	egressSettle = 2 * time.Second

	// Exit statuses of the clients.
	exitConnectRefused = 1  // caboose-agent connect, any error
	exitCurlHTTP       = 22 // curl -f, an HTTP error status
	exitCurlConnect    = 7  // curl, could not connect
	exitCurlTimeout    = 28 // curl, timed out
	exitNotRun         = 126
	exitNotFound       = 127

	// gapOpen and gapClosed are E8's reasons: what the threat model
	// documents, and what it says when the gap is found closed.
	gapOpen   = "egress unrestricted (documented)"
	gapClosed = "closed: update docs/security.md"
)

// claudeHostNames are the hosts a hostile launch requires egress_allow to
// name. The positive control of E7 is that naming them all is enough: a
// host the launcher added since makes it an ERROR naming that host.
var claudeHostNames = []string{"api.anthropic.com", "claude.ai", "platform.claude.com", "downloads.claude.ai"}

// EgressChecks are E1-E8 in the order they run.
func EgressChecks() []c.Check {
	return []c.Check{e1(), e2(), e3(), e4(), e5(), e6(), e7(), e8()}
}

func vmOnly(ck c.Check) c.Check {
	ck.Isolations = []string{"vm"}
	return ck
}

// egressRun is one egress check's run.
type egressRun struct {
	env  c.Env
	host c.HostOps
	ops  c.EgressOps
	id   string
	orig c.EgressPolicy

	listeners []c.SuiteListener
	// made is how many connections the check meant each listener to get:
	// its positive controls'.
	made map[c.SuiteListener]int
	// refused are the tokens of the attempts that must not arrive.
	refused []string
	attempt int
	lines   []string
	// changed is whether the check changed the egress keys.
	changed bool
}

// client is a way the sandbox connects through the outbound proxy.
type client struct {
	name  string
	build func(c.Target, string) (c.Cmd, error)
	// refused reports whether the result is the proxy's refusal (403).
	refused func(c.ExecResult) bool
}

var proxyClients = []client{
	{"caboose-agent connect", c.AgentConnect, func(r c.ExecResult) bool {
		return r.RC == exitConnectRefused && strings.Contains(r.Msg, "through the host (403)")
	}},
	{"curl through the proxy", c.CurlProxy, func(r c.ExecResult) bool {
		return r.RC == exitCurlHTTP && strings.Contains(r.Msg, "error: 403")
	}},
}

func egressCheck(id, title, control string, class c.Class, run func(context.Context, *egressRun) c.CheckResult) c.Check {
	return c.Check{
		ID: id, Title: title, Control: control, Step: stepEgress, Group: groupEgress, Class: class,
		Run: func(ctx context.Context, env c.Env) (out c.CheckResult) {
			e, bad := prepareEgress(env, id)
			if bad != nil {
				return *bad
			}
			defer func() {
				for _, l := range e.listeners {
					if err := l.Close(); err != nil && out.Status != c.Error {
						out = errorf("the suite's listener at %s did not close: %v", l.Addr(), err)
					}
				}
			}()
			defer func() {
				if r := e.restore(); r != nil {
					out = *r
				}
			}()
			r := run(ctx, e)
			r.Lines = append(e.lines, r.Lines...)
			return r
		},
	}
}

// prepareEgress is what every egress check needs before anything is sent
// or changed: a running sandbox, the host's calls, the stubs in place, and
// the profile's egress keys as they are, to put back.
func prepareEgress(env c.Env, id string) (*egressRun, *c.CheckResult) {
	bad := func(r c.CheckResult) (*egressRun, *c.CheckResult) { return nil, &r }
	if st := env.State(); st != c.StateRunning {
		return bad(errorf("the sandbox is not running (%s)", st))
	}
	host, err := env.HostOps()
	if err != nil {
		return bad(errorf("%v", err))
	}
	if err := host.StubsInPlace(); err != nil {
		return bad(errorf("the suite's stub opener and notifier are not in place, so nothing was sent: %v", err))
	}
	ops, err := env.EgressOps()
	if err != nil {
		return bad(errorf("%v", err))
	}
	orig, err := ops.EgressKeys()
	if err != nil {
		return bad(errorf("the configuration's egress keys could not be read: %v", err))
	}
	return &egressRun{env: env, host: host, ops: ops, id: id, orig: orig, made: map[c.SuiteListener]int{}}, nil
}

// restore puts the egress keys back as they were, and says so when it
// cannot: the run's configuration is then no longer the audited one.
func (e *egressRun) restore() *c.CheckResult {
	if !e.changed {
		return nil
	}
	err := e.ops.SetEgress(e.orig)
	if err == nil {
		var now c.EgressPolicy
		if now, err = e.ops.EgressKeys(); err == nil && !now.Equal(e.orig) {
			err = errors.New("they differ from the run's")
		}
	}
	if err != nil {
		r := errorf("the egress keys of config.toml were not put back as they were (%v); the run's configuration is no longer the audited one", err)
		return &r
	}
	return nil
}

// listen starts a suite listener at scope. A missing LAN address is a
// SKIP.
func (e *egressRun) listen(ctx context.Context, scope c.Scope) (c.SuiteListener, c.Target, *c.CheckResult) {
	l, err := e.ops.StartListener(ctx, scope)
	if errors.Is(err, c.ErrNoLAN) {
		r := c.CheckResult{Status: c.Skip, Reason: "this machine has no private address for the suite's listener"}
		return nil, c.Target{}, &r
	}
	if err != nil {
		r := errorf("the suite's %s listener could not start: %v", scope, err)
		return nil, c.Target{}, &r
	}
	e.listeners = append(e.listeners, l)
	t, err := c.TargetOf(l)
	if err != nil {
		r := errorf("%v", err)
		return nil, c.Target{}, &r
	}
	return l, t, nil
}

func (e *egressRun) set(p c.EgressPolicy) *c.CheckResult {
	e.changed = true
	if err := e.ops.SetEgress(p); err != nil {
		r := errorf("the egress keys of config.toml could not be changed: %v", err)
		return &r
	}
	return nil
}

// policy is the run's egress keys with allow and deny added, and the
// ports set to ports.
func (e *egressRun) policy(allow, deny []string, ports ...int) c.EgressPolicy {
	return c.EgressPolicy{
		Allow: append(slices.Clone(e.orig.Allow), allow...),
		Deny:  append(slices.Clone(e.orig.Deny), deny...),
		Ports: ports,
	}
}

func (e *egressRun) token() (string, error) {
	e.attempt++
	return c.Token(e.id, e.attempt)
}

func (e *egressRun) counts() map[c.SuiteListener]int {
	m := map[c.SuiteListener]int{}
	for _, l := range e.listeners {
		m[l] = l.Accepted()
	}
	return m
}

func ran(r c.ExecResult) bool {
	return r.RC >= 0 && r.RC != exitNotRun && r.RC != exitNotFound
}

// exec runs cmd as user and requires the shell to have run it.
func (e *egressRun) exec(ctx context.Context, user string, cmd c.Cmd, err error, what string) (c.ExecResult, *c.CheckResult) {
	if err != nil {
		r := errorf("%v", err)
		return c.ExecResult{}, &r
	}
	res, err := e.env.RunCmd(ctx, user, cmd)
	if err != nil {
		r := errorf("%s could not be run: %v", what, err)
		return res, &r
	}
	if !ran(res) {
		r := errorf("%s did not run (exit %d)%s", what, res.RC, msgOf(res))
		return res, &r
	}
	return res, nil
}

// reach is a positive control: each client's connection to l at t
// arrives, as the listener sees it.
func (e *egressRun) reach(ctx context.Context, l c.SuiteListener, t c.Target) *c.CheckResult {
	for _, cl := range proxyClients {
		tok, err := e.token()
		cmd, berr := cl.build(t, tok)
		if berr == nil {
			berr = err
		}
		res, bad := e.exec(ctx, e.env.SessionUser(), cmd, berr, cl.name)
		if bad != nil {
			b := errorf("positive control failed: %s", bad.Reason)
			return &b
		}
		if !received(l, tok) {
			r := errorf("positive control failed: %s to %s, allowed, did not reach the suite's listener (exit %d%s)", cl.name, t, res.RC, msgOf(res))
			return &r
		}
		e.made[l]++
	}
	return nil
}

func received(l c.SuiteListener, tok string) bool {
	for _, line := range l.Received() {
		// The CONNECT client sends the token as its line; curl asks for
		// it as the path.
		if line == tok || strings.HasPrefix(line, "GET /"+tok+" ") {
			return true
		}
	}
	return false
}

// refuse is an attempt that must be refused: each client's connection to
// t, which reaches no listener of the check's and is refused by the
// proxy. want, when set, is what the refusal of the CONNECT client must
// name (curl's -f shows the status alone).
func (e *egressRun) refuse(ctx context.Context, t c.Target, what, want string) *c.CheckResult {
	for _, cl := range proxyClients {
		tok, err := e.token()
		cmd, berr := cl.build(t, tok)
		if berr == nil {
			berr = err
		}
		e.refused = append(e.refused, tok)
		before := e.counts()
		res, bad := e.exec(ctx, e.env.SessionUser(), cmd, berr, cl.name)
		if r := e.arrived(before, cl.name, t, what); r != nil {
			return r
		}
		if bad != nil {
			return bad
		}
		switch {
		case cl.refused(res):
			if want != "" && cl.name == proxyClients[0].name && !strings.Contains(res.Msg, want) {
				r := errorf("%s to %s (%s) was refused, but not for %s%s", cl.name, t, what, want, msgOf(res))
				return &r
			}
			e.lines = append(e.lines, fmt.Sprintf("%s to %s (%s): refused%s", cl.name, t, what, reasonOf(res)))
		case res.RC == 0:
			r := failf("%s to %s (%s): the sandbox reported a connection", cl.name, t, what)
			return &r
		default:
			r := errorf("%s to %s (%s) ended with exit %d%s, which is not the proxy's refusal", cl.name, t, what, res.RC, msgOf(res))
			return &r
		}
	}
	return nil
}

// arrived is a FAIL when any of the check's listeners got a connection
// since before.
func (e *egressRun) arrived(before map[c.SuiteListener]int, by string, t c.Target, what string) *c.CheckResult {
	for _, l := range e.listeners {
		if n := l.Accepted(); n != before[l] {
			r := failf("%s to %s (%s) reached the suite's listener at %s", by, t, what, l.Addr())
			return &r
		}
	}
	return nil
}

// settle waits, then holds every listener to the connections the check
// made: one more is a connection the host let through late, or one from
// elsewhere (both FAIL: the suite cannot tell them apart).
func (e *egressRun) settle(ctx context.Context) *c.CheckResult {
	if err := e.host.Wait(ctx, egressSettle); err != nil {
		r := errorf("%v", err)
		return &r
	}
	for _, l := range e.listeners {
		if n := l.Accepted(); n != e.made[l] {
			r := failf("the suite's listener at %s accepted %d connection(s); the check's positive controls made %d", l.Addr(), n, e.made[l])
			return &r
		}
		for _, tok := range e.refused {
			if received(l, tok) {
				r := failf("a connection that was refused arrived at the suite's listener at %s later", l.Addr())
				return &r
			}
		}
	}
	return nil
}

// reasonOf is the refusal's reason as the host gave it: pending, the
// rule that refused.
func reasonOf(r c.ExecResult) string {
	for _, k := range []string{"pending", "egress_deny", "egress_ports", "own", "private"} {
		if strings.Contains(r.Msg, k) {
			return " (" + k + ")"
		}
	}
	return ""
}

// first is the first non-nil result.
func first(rs ...func() *c.CheckResult) *c.CheckResult {
	for _, r := range rs {
		if x := r(); x != nil {
			return x
		}
	}
	return nil
}

func e1() c.Check {
	return vmOnly(egressCheck("E1", "loopback and the host's own addresses are never reached through the proxy",
		"the proxy refuses this machine's own addresses", c.Write,
		func(ctx context.Context, e *egressRun) c.CheckResult {
			lo, tlo, r := e.listen(ctx, c.ScopeLoopback)
			if r != nil {
				return *r
			}
			lan, tlan, r := e.listen(ctx, c.ScopeLAN)
			if r != nil && r.Status != c.Skip {
				return *r
			}
			hasLAN := r == nil
			// The positive control: an egress_allow address within the
			// range lets each through.
			allow, ports := []string{tlo.Addr().String()}, []int{tlo.Port()}
			if hasLAN {
				allow, ports = append(allow, tlan.Addr().String()), append(ports, tlan.Port())
			}
			if r := e.set(e.policy(allow, nil, ports...)); r != nil {
				return *r
			}
			if r := e.reach(ctx, lo, tlo); r != nil {
				return *r
			}
			if hasLAN {
				if r := e.reach(ctx, lan, tlan); r != nil {
					return *r
				}
			}
			// The attempt: egress_allow names them otherwise (the name
			// localhost, a network wider than the LAN address), which must
			// not let them through.
			byName, err := tlo.ByName()
			if err != nil {
				return errorf("%v", err)
			}
			allow = []string{"localhost"}
			var cidr string
			if hasLAN {
				if cidr, err = tlan.CIDR(); err != nil {
					return errorf("%v", err)
				}
				allow = append(allow, cidr)
			}
			if r := e.set(e.policy(allow, nil, ports...)); r != nil {
				return *r
			}
			if r := first(
				func() *c.CheckResult { return e.refuse(ctx, byName, "loopback by a name egress_allow names", "") },
				func() *c.CheckResult { return e.refuse(ctx, tlo, "loopback by address", "") },
				func() *c.CheckResult {
					if !hasLAN {
						return nil
					}
					return e.refuse(ctx, tlan, "this machine's own address, in a network egress_allow names", "")
				},
				func() *c.CheckResult { return e.settle(ctx) },
			); r != nil {
				return *r
			}
			if !hasLAN {
				return pass("not tried: this machine's own LAN address (it has no private address)")
			}
			return pass()
		}))
}

func e2() c.Check {
	return vmOnly(egressCheck("E2", "egress to a private address is refused unless allowed", "egress denied unless allowed", c.Write,
		func(ctx context.Context, e *egressRun) c.CheckResult {
			lan, t, r := e.listen(ctx, c.ScopeLAN)
			if r != nil {
				return *r
			}
			if r := first(
				func() *c.CheckResult { return e.set(e.policy([]string{t.Addr().String()}, nil, t.Port())) },
				func() *c.CheckResult { return e.reach(ctx, lan, t) },
				func() *c.CheckResult { return e.set(e.policy(nil, nil, t.Port())) },
				func() *c.CheckResult { return e.refuse(ctx, t, "a private address egress_allow does not name", "") },
				func() *c.CheckResult { return e.settle(ctx) },
			); r != nil {
				return *r
			}
			return pass("the listener's address is one of this machine's own, so the refusal may be that rule's rather than a pending request",
				"not tried: a private address that is not this machine's, which the suite has no listener on")
		}))
}

func e3() c.Check {
	return vmOnly(egressCheck("E3", "allowed means allowed, and only that", "egress allowed only as configured", c.Write,
		func(ctx context.Context, e *egressRun) c.CheckResult {
			a, ta, r := e.listen(ctx, c.ScopeLAN)
			if r != nil {
				return *r
			}
			_, tb, r := e.listen(ctx, c.ScopeLAN)
			if r != nil {
				return *r
			}
			_, tc, r := e.listen(ctx, c.ScopeLoopback)
			if r != nil {
				return *r
			}
			if ta.Addr() != tb.Addr() {
				return errorf("the suite's two LAN listeners are on different addresses")
			}
			if r := first(
				func() *c.CheckResult {
					return e.set(e.policy([]string{ta.Addr().String()}, nil, ta.Port(), tc.Port()))
				},
				func() *c.CheckResult { return e.reach(ctx, a, ta) },
				func() *c.CheckResult {
					return e.refuse(ctx, tb, "the allowed address, at a port egress_ports does not hold", "egress_ports")
				},
				func() *c.CheckResult {
					return e.refuse(ctx, tc, "another address, at a port egress_ports holds", "")
				},
				func() *c.CheckResult { return e.settle(ctx) },
			); r != nil {
				return *r
			}
			return pass("the other address is the loopback listener, which the rule for this machine's own addresses refuses too",
				"not tried: an ordinary private address beside the allowed one, which the suite has no listener on")
		}))
}

func e4() c.Check {
	return vmOnly(egressCheck("E4", "the guest cannot go around the proxy", "the guest has no network but the proxy", c.Write,
		func(ctx context.Context, e *egressRun) c.CheckResult {
			lan, t, r := e.listen(ctx, c.ScopeLAN)
			if r != nil {
				return *r
			}
			if e.env.Trust() != c.TrustHostile {
				return e4Gap(ctx, e, lan, t)
			}
			doc, err := c.DocTarget(t)
			if err != nil {
				return errorf("%v", err)
			}
			if r := first(
				func() *c.CheckResult { return e.set(e.policy([]string{t.Addr().String()}, nil, t.Port())) },
				func() *c.CheckResult { return e.reach(ctx, lan, t) },
				// The routes first: a guest that has one sends nothing more.
				func() *c.CheckResult { return e.routes(ctx) },
				func() *c.CheckResult { return e.direct(ctx, t, "the LAN listener") },
				func() *c.CheckResult { return e.direct(ctx, doc, "the documentation address") },
				func() *c.CheckResult { return e.ping(ctx, t) },
				func() *c.CheckResult { return e.settle(ctx) },
			); r != nil {
				return *r
			}
			return pass("not tried: a raw socket other than ping's")
		}))
}

// e4Gap is E4 under fallible, where the guest's NAT is on by design: the
// line records whether the guest reached the LAN listener directly.
func e4Gap(ctx context.Context, e *egressRun, lan c.SuiteListener, t c.Target) c.CheckResult {
	tok, err := e.token()
	cmd, berr := c.CurlDirect(t, tok)
	if berr == nil {
		berr = err
	}
	if _, bad := e.exec(ctx, e.env.SessionUser(), cmd, berr, "curl around the proxy"); bad != nil {
		return *bad
	}
	if err := e.host.Wait(ctx, egressSettle); err != nil {
		return errorf("%v", err)
	}
	if received(lan, tok) {
		e.made[lan]++
		return c.CheckResult{Status: c.Gap, Reason: "under fallible the guest has its NAT, and reaches this machine's network around the proxy (documented)"}
	}
	e.refused = append(e.refused, tok)
	return c.CheckResult{Status: c.Gap, Reason: "under fallible the guest has its NAT (documented), but did not reach the LAN listener around the proxy"}
}

// direct is an attempt around the proxy, which must not connect.
func (e *egressRun) direct(ctx context.Context, t c.Target, what string) *c.CheckResult {
	tok, err := e.token()
	cmd, berr := c.CurlDirect(t, tok)
	if berr == nil {
		berr = err
	}
	e.refused = append(e.refused, tok)
	before := e.counts()
	res, bad := e.exec(ctx, e.env.SessionUser(), cmd, berr, "curl around the proxy")
	if r := e.arrived(before, "curl around the proxy", t, what); r != nil {
		return r
	}
	if bad != nil {
		return bad
	}
	switch res.RC {
	case exitCurlConnect, exitCurlTimeout:
		e.lines = append(e.lines, fmt.Sprintf("curl around the proxy to %s (%s): no connection%s", t, what, msgOf(res)))
		return nil
	case 0, exitCurlHTTP:
		r := failf("curl around the proxy to %s (%s): the sandbox reported a connection", t, what)
		return &r
	}
	r := errorf("curl around the proxy to %s (%s) ended with exit %d%s, which is not a failure to connect", t, what, res.RC, msgOf(res))
	return &r
}

func (e *egressRun) ping(ctx context.Context, t c.Target) *c.CheckResult {
	cmd, err := c.Ping(t)
	res, bad := e.exec(ctx, "root", cmd, err, "ping")
	if bad != nil {
		return bad
	}
	if res.RC == 0 {
		r := failf("ping from the guest to %s was answered", t.Addr())
		return &r
	}
	e.lines = append(e.lines, fmt.Sprintf("ping to %s: no answer%s", t.Addr(), msgOf(res)))
	return nil
}

// routeTypes are the route types ip prints before a route's prefix.
var routeTypes = []string{"unicast", "local", "broadcast", "multicast", "anycast", "unreachable", "prohibit", "blackhole", "throw", "nat"}

// routes is E4's look at the hostile guest's network: no NIC at all (by
// /sys/class/net and by what the virtio_net driver has bound, which no
// network namespace hides), no default route in any table of either
// family, and no route through a device the interface list does not
// name. What is left goes through virtual devices of the guest's own (lo,
// its dockerd's docker0), which reach nothing beyond the VM without a
// NIC; each but lo's is a line.
func (e *egressRun) routes(ctx context.Context) *c.CheckResult {
	devs, bad := e.exec(ctx, e.env.SessionUser(), c.NetDevices(), nil, "the network interfaces' list")
	if bad != nil {
		return bad
	}
	virtual, nics := map[string]bool{}, []string{}
	for _, line := range strings.Split(devs.Out, "\n") {
		kind, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		switch {
		case !ok && kind == "":
		case ok && (kind == "nic" || kind == "virtio-net") && name != "":
			nics = append(nics, kind+" "+name)
		case ok && kind == "virtual" && name != "":
			virtual[name] = true
		default:
			r := errorf("the network interfaces' list has a line it should not: %q", line)
			return &r
		}
	}
	if devs.RC != 0 || !virtual["lo"] {
		r := errorf("the network interfaces could not be listed (exit %d, loopback not among them)%s", devs.RC, msgOf(devs))
		return &r
	}
	if len(nics) > 0 {
		r := failf("the guest has a network device: %s", strings.Join(nics, ", "))
		return &r
	}
	res, bad := e.exec(ctx, e.env.SessionUser(), c.Routes(), nil, "ip route")
	if bad != nil {
		return bad
	}
	if res.RC != 0 {
		r := errorf("ip route ended with exit %d%s", res.RC, msgOf(res))
		return &r
	}
	for _, line := range strings.Split(res.Out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.TrimSpace(line) == c.NoIPv6 {
			continue
		}
		if strings.HasPrefix(f[0], "Error:") || strings.HasPrefix(f[0], "ip:") || f[0] == "RTNETLINK" {
			r := errorf("ip route complained: %s", line)
			return &r
		}
		dest := f[0]
		if slices.Contains(routeTypes, dest) && len(f) > 1 {
			dest = f[1]
		}
		dev := ""
		if i := slices.Index(f, "dev"); i >= 0 && i+1 < len(f) {
			dev = f[i+1]
		}
		switch {
		case dest == "default" && dev != "lo" && !slices.Contains([]string{"unreachable", "prohibit", "blackhole", "throw"}, f[0]):
			r := failf("the guest has a default route: %s", line)
			return &r
		case dev == "lo":
		case virtual[dev]:
			e.lines = append(e.lines, fmt.Sprintf("a route through %s, a virtual device of the guest's own, with no NIC in the guest: %s", dev, line))
		case dev == "" && slices.Contains([]string{"unreachable", "prohibit", "blackhole", "throw"}, f[0]):
		default:
			r := failf("the guest has a route through a device it does not list: %s", line)
			return &r
		}
	}
	return nil
}

func e5() c.Check {
	return vmOnly(c.Check{ID: "E5", Title: "DNS cannot carry a connection", Control: "names are resolved by the host",
		Step: stepEgress, Group: groupEgress, Class: c.Observe,
		Run: func(context.Context, c.Env) c.CheckResult {
			return c.CheckResult{Status: c.Skip, Reason: "no name can point at the suite's listener without changing this machine's resolver configuration, which the suite never changes"}
		}})
}

func e6() c.Check {
	return vmOnly(egressCheck("E6", "egress_deny beats egress_allow", "deny beats allow", c.Write,
		func(ctx context.Context, e *egressRun) c.CheckResult {
			lan, t, r := e.listen(ctx, c.ScopeLAN)
			if r != nil {
				return *r
			}
			a := []string{t.Addr().String()}
			if r := first(
				func() *c.CheckResult { return e.set(e.policy(a, nil, t.Port())) },
				func() *c.CheckResult { return e.reach(ctx, lan, t) },
				func() *c.CheckResult { return e.set(e.policy(a, a, t.Port())) },
				func() *c.CheckResult {
					return e.refuse(ctx, t, "an address both egress_allow and egress_deny name", "egress_deny")
				},
				func() *c.CheckResult { return e.settle(ctx) },
			); r != nil {
				return *r
			}
			return pass()
		}))
}

func e7() c.Check {
	ck := vmOnly(c.Check{ID: "E7", Title: "a hostile launch needs Claude Code's hosts in egress_allow", Control: "Claude Code's hosts rule",
		Step: stepEgress, Group: groupEgress, Class: c.Observe,
		Run: func(ctx context.Context, env c.Env) c.CheckResult {
			ops, err := env.EgressOps()
			if err != nil {
				return errorf("%v", err)
			}
			orig, err := ops.EgressKeys()
			if err != nil {
				return errorf("the configuration's egress keys could not be read: %v", err)
			}
			with := func(allow, deny []string) c.EgressPolicy {
				return c.EgressPolicy{Allow: allow, Deny: append(slices.Clone(orig.Deny), deny...), Ports: orig.Ports}
			}
			res, err := ops.LaunchDecision(ctx, with(slices.Clone(claudeHostNames), nil))
			switch {
			case err != nil:
				return errorf("positive control failed: the launcher's decision could not be run: %v", err)
			case res.Refused:
				return errorf("positive control failed: a launch with every host the suite knows of is refused: %s", res.Message)
			case res.Created:
				return failf("the launcher's decision created a sandbox")
			}
			for _, h := range claudeHostNames {
				allow := slices.DeleteFunc(slices.Clone(claudeHostNames), func(x string) bool { return x == h })
				if r := e7Refused(ctx, ops, with(allow, nil), h, "egress_allow without it"); r != nil {
					return *r
				}
			}
			h := claudeHostNames[0]
			if r := e7Refused(ctx, ops, with(slices.Clone(claudeHostNames), []string{h}), h, "egress_deny naming it"); r != nil {
				return *r
			}
			return pass("not tried: egress_ports without 443, and *.suffix patterns naming the hosts")
		}})
	ck.Trusts = []string{c.TrustHostile}
	return ck
}

func e7Refused(ctx context.Context, ops c.EgressOps, p c.EgressPolicy, h, what string) *c.CheckResult {
	res, err := ops.LaunchDecision(ctx, p)
	var r c.CheckResult
	switch {
	case err != nil:
		r = errorf("the launcher's decision could not be run: %v", err)
	case res.Created:
		r = failf("the launcher's decision created a sandbox")
	case !res.Refused:
		r = failf("a hostile launch with %s, %s, is not refused", h, what)
	case !strings.Contains(res.Message, h):
		r = failf("a hostile launch with %s, %s, is refused without naming it: %s", h, what, res.Message)
	default:
		return nil
	}
	return &r
}

func e8() c.Check {
	ck := egressCheck("E8", "egress under container and gVisor", "container and gVisor egress", c.Observe,
		func(ctx context.Context, e *egressRun) c.CheckResult {
			lan, t, r := e.listen(ctx, c.ScopeLAN)
			if r != nil {
				return *r
			}
			tok, err := e.token()
			cmd, berr := c.CurlDirect(t, tok)
			if berr == nil {
				berr = err
			}
			if _, bad := e.exec(ctx, e.env.SessionUser(), cmd, berr, "curl"); bad != nil {
				return *bad
			}
			if err := e.host.Wait(ctx, egressSettle); err != nil {
				return errorf("%v", err)
			}
			if received(lan, tok) {
				return c.CheckResult{Status: c.Gap, Reason: gapOpen}
			}
			return c.CheckResult{Status: c.Gap, Reason: gapClosed}
		})
	ck.Isolations = []string{"container", "gvisor"}
	ck.Trusts = []string{c.TrustFallible}
	return ck
}
