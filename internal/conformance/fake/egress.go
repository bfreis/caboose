package fake

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/conformance"
)

// The mutants of the outbound proxy, the guest's network and the
// launcher's Claude Code hosts rule, which Egress acts on, beside
// EgressOpen of the model's own list.
const (
	// LoopbackByName lets a name egress_allow names reach loopback.
	LoopbackByName Mutant = "loopback reached by an allowed name"
	// OwnByNetwork lets this machine's own address through for a network
	// egress_allow names.
	OwnByNetwork Mutant = "own address reached by an allowed network"
	// UnnamedAllowed lets through what egress_allow does not name: the
	// rules for loopback, this machine's own and private addresses are
	// skipped.
	UnnamedAllowed Mutant = "unnamed destinations allowed"
	// PortsIgnored lets every port through.
	PortsIgnored Mutant = "egress ports ignored"
	// AllowBeatsDeny reads egress_allow before egress_deny.
	AllowBeatsDeny Mutant = "allow beats deny"
	// ProxyLies connects what it refuses, and still says 403.
	ProxyLies Mutant = "proxy connects what it refuses"
	// ProxyLate connects what it refuses a moment later.
	ProxyLate Mutant = "proxy connects late"
	// GuestNAT gives a hostile guest the NAT: a network around the proxy.
	GuestNAT Mutant = "hostile guest has a NAT"
	// GuestRoute gives a hostile guest a default route, and nothing else.
	GuestRoute Mutant = "hostile guest has a route"
	// GuestNICRoute gives a hostile guest a NIC and a route through it,
	// but no default route.
	GuestNICRoute Mutant = "hostile guest has a NIC route"
	// GuestNICBesideBridge gives a hostile guest a NIC with no route of
	// its own beside the docker0 route every guest has: a bridge route is
	// no exit only while no NIC is there.
	GuestNICBesideBridge Mutant = "hostile guest has a NIC beside docker0"
	// GuestNICInOtherNamespace hides a hostile guest's NIC from
	// /sys/class/net (another network namespace holds it); the virtio_net
	// driver still has it bound.
	GuestNICInOtherNamespace Mutant = "hostile guest has a NIC in another namespace"
	// GuestV6Default gives a hostile guest an IPv6 default route alone.
	GuestV6Default Mutant = "hostile guest has an IPv6 default route"
	// GuestUnlistedRoute gives a hostile guest a route through a device
	// the interface list does not name.
	GuestUnlistedRoute Mutant = "hostile guest has a route through an unlisted device"
	// GuestPing answers a hostile guest's ping.
	GuestPing Mutant = "hostile guest is answered"
	// ClaudeHostsUnchecked launches without Claude Code's hosts.
	ClaudeHostsUnchecked Mutant = "claude hosts unchecked"
	// ClaudeDenyIgnored launches with a Claude Code host in egress_deny.
	ClaudeDenyIgnored Mutant = "claude hosts deny ignored"
	// ClaudeRefusalUnnamed refuses without naming the host.
	ClaudeRefusalUnnamed Mutant = "claude hosts refusal unnamed"
	// DecisionCreates creates a sandbox on a launch decision.
	DecisionCreates Mutant = "launch decision creates"
)

// EgressMutants are the mutants Egress acts on.
var EgressMutants = []Mutant{
	EgressOpen, LoopbackByName, OwnByNetwork, UnnamedAllowed, PortsIgnored, AllowBeatsDeny,
	ProxyLies, ProxyLate, GuestNAT, GuestRoute, GuestNICRoute, GuestNICBesideBridge, GuestNICInOtherNamespace, GuestV6Default, GuestUnlistedRoute, GuestPing,
	ClaudeHostsUnchecked, ClaudeDenyIgnored, ClaudeRefusalUnnamed, DecisionCreates,
}

// The faults Egress acts on: the setup's, or answers of the product's a
// check was not written for.
const (
	ProxyDown        Fault = "proxy down"
	AllowIgnored     Fault = "allow ignored"
	SetEgressFails   Fault = "set egress fails"
	RestoreFails     Fault = "restore fails"
	EgressUnreadable Fault = "egress unreadable"
	ListenFails      Fault = "listen fails"
	NoLAN            Fault = "no LAN address"
	CurlMissing      Fault = "curl missing"
	PingMissing      Fault = "ping missing"
	RoutesFail       Fault = "ip route fails"
	DevicesFail      Fault = "network interfaces not listed"
	// RoutesComplain: ip says what it could not do on its output, and
	// exits 0.
	RoutesComplain   Fault = "ip route complains"
	RefusalUnnamed   Fault = "refusal unnamed"
	ClaudeRefused    Fault = "claude hosts always refused"
	LaunchFails      Fault = "launch decision fails"
	StrayConnection  Fault = "stray connection"
	ContainerBlocked Fault = "container egress blocked"
	// LauncherNeedsMore: the launcher requires a host the check does not
	// know of.
	LauncherNeedsMore Fault = "launcher needs another host"
	// SandboxClaimsConnect: the sandbox says it connected where the proxy
	// refused, and no listener saw it (another service answered, say).
	SandboxClaimsConnect Fault = "sandbox claims a connection"
	// NATBlocked: a fallible guest's NAT does not reach this machine.
	NATBlocked Fault = "NAT blocked"
)

// EgressFaults are every fault Egress acts on.
var EgressFaults = []Fault{ProxyDown, AllowIgnored, SetEgressFails, RestoreFails, EgressUnreadable,
	ListenFails, NoLAN, CurlMissing, PingMissing, RoutesFail, DevicesFail, RoutesComplain, RefusalUnnamed, ClaudeRefused,
	LaunchFails, StrayConnection, ContainerBlocked, LauncherNeedsMore,
	SandboxClaimsConnect, NATBlocked}

// The model's addresses: this machine's loopback, and its own LAN address.
var (
	modelLoopback = netip.MustParseAddr("127.0.0.1")
	modelLAN      = netip.MustParseAddr("192.168.1.20")
	modelDoc      = netip.MustParseAddr("192.0.2.1")
)

// moreHost is the host the model's launcher requires beside the ones it
// is given, under the fault LauncherNeedsMore.
const moreHost = "telemetry.example"

// Egress is a Link with the outbound proxy, the guest's own network, the
// suite's listeners and the launcher's Claude Code hosts rule: what the
// egress checks talk to. Nothing in it binds or connects anything.
type Egress struct {
	*Link

	em     sync.Mutex
	policy conformance.EgressPolicy
	sets   int
	// hosts are the hosts the model's launcher requires egress_allow to
	// name under hostile.
	hosts     []string
	listeners []*modelListener
	nextPort  int
	late      []*modelListener
	stray     bool
	// opened are the listeners StartListener made, closed or not.
	opened []*modelListener
	// targets are every host:port a command named.
	targets []string
}

// NewEgress is an Egress over a new Link, whose launcher requires hosts
// in egress_allow under hostile (and the configuration names them).
func NewEgress(n8, isolation, trust string, hosts []string) (*Egress, error) {
	l, err := NewLink(n8, isolation, trust)
	if err != nil {
		return nil, err
	}
	e := &Egress{Link: l, nextPort: 40001, hosts: slices.Clone(hosts)}
	if trust == conformance.TrustHostile {
		e.policy.Allow = slices.Clone(hosts)
	}
	return e, nil
}

// Env is a conformance.Env over the Egress.
func (e *Egress) Env(isolation, trust string) conformance.Env {
	return conformance.NewEnv(e, e, e, e, isolation, trust)
}

func (e *Egress) fault(f Fault) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.faults[f]
}

func (e *Egress) mutant(m Mutant) bool { return e.Active(m) }

// Listeners are every listener StartListener made.
func (e *Egress) Listeners() []conformance.SuiteListener {
	e.em.Lock()
	defer e.em.Unlock()
	var out []conformance.SuiteListener
	for _, l := range e.opened {
		out = append(out, l)
	}
	return out
}

// OpenListeners is how many listeners are not closed.
func (e *Egress) OpenListeners() int {
	e.em.Lock()
	defer e.em.Unlock()
	n := 0
	for _, l := range e.opened {
		if !l.closed {
			n++
		}
	}
	return n
}

// Targets are every host:port a command sent named.
func (e *Egress) Targets() []string {
	e.em.Lock()
	defer e.em.Unlock()
	return slices.Clone(e.targets)
}

// Policy is the egress keys now.
func (e *Egress) Policy() conformance.EgressPolicy {
	e.em.Lock()
	defer e.em.Unlock()
	return e.policy
}

type modelListener struct {
	e        *Egress
	addr     netip.Addr
	port     int
	accepted int
	received []string
	closed   bool
}

func (l *modelListener) Addr() string {
	return netip.AddrPortFrom(l.addr, uint16(l.port)).String()
}

func (l *modelListener) Accepted() int {
	l.e.em.Lock()
	defer l.e.em.Unlock()
	return l.accepted
}

func (l *modelListener) Received() []string {
	l.e.em.Lock()
	defer l.e.em.Unlock()
	return slices.Clone(l.received)
}

func (l *modelListener) Close() error {
	l.e.em.Lock()
	defer l.e.em.Unlock()
	l.closed = true
	return nil
}

// EgressOps.

func (e *Egress) StartListener(ctx context.Context, scope conformance.Scope) (conformance.SuiteListener, error) {
	if e.fault(ListenFails) {
		return nil, errors.New("address already in use")
	}
	var a netip.Addr
	switch scope {
	case conformance.ScopeLoopback:
		a = modelLoopback
	case conformance.ScopeLAN:
		if e.fault(NoLAN) {
			return nil, conformance.ErrNoLAN
		}
		a = modelLAN
	default:
		return nil, fmt.Errorf("no scope %q", scope)
	}
	e.em.Lock()
	defer e.em.Unlock()
	l := &modelListener{e: e, addr: a, port: e.nextPort}
	e.nextPort++
	e.listeners = append(e.listeners, l)
	e.opened = append(e.opened, l)
	return l, nil
}

func (e *Egress) EgressKeys() (conformance.EgressPolicy, error) {
	if e.fault(EgressUnreadable) {
		return conformance.EgressPolicy{}, errors.New("config.toml cannot be read")
	}
	e.em.Lock()
	defer e.em.Unlock()
	return clonePolicy(e.policy), nil
}

func clonePolicy(p conformance.EgressPolicy) conformance.EgressPolicy {
	return conformance.EgressPolicy{Allow: slices.Clone(p.Allow), Deny: slices.Clone(p.Deny), Ports: slices.Clone(p.Ports)}
}

func (e *Egress) SetEgress(p conformance.EgressPolicy) error {
	if e.fault(SetEgressFails) {
		return errors.New("config.toml is read-only")
	}
	e.em.Lock()
	defer e.em.Unlock()
	e.sets++
	if e.sets > 1 && e.fault(RestoreFails) {
		return errors.New("config.toml is read-only now")
	}
	e.policy = clonePolicy(p)
	return nil
}

func (e *Egress) LaunchDecision(ctx context.Context, p conformance.EgressPolicy) (conformance.LaunchResult, error) {
	if e.fault(LaunchFails) {
		return conformance.LaunchResult{}, errors.New("the configuration does not load")
	}
	res := conformance.LaunchResult{Created: e.mutant(DecisionCreates)}
	if e.fault(ClaudeRefused) {
		res.Refused, res.Message = true, "trust = hostile: refused"
		return res, nil
	}
	var missing, denied []string
	hosts := e.hosts
	if e.fault(LauncherNeedsMore) {
		hosts = append(slices.Clone(hosts), moreHost)
	}
	for _, h := range hosts {
		switch {
		case slices.Contains(p.Deny, h) && !e.mutant(ClaudeDenyIgnored):
			denied = append(denied, h)
		case !slices.Contains(p.Allow, h) && !e.mutant(ClaudeHostsUnchecked):
			missing = append(missing, h)
		}
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "trust = hostile: egress_allow does not name "+strings.Join(missing, ", ")+", which Claude Code needs to start and log in")
	}
	if len(denied) > 0 {
		parts = append(parts, "egress_deny names "+strings.Join(denied, ", ")+", which Claude Code needs: remove it from egress_deny")
	}
	if len(parts) > 0 {
		res.Refused, res.Message = true, strings.Join(parts, "; ")
		if e.mutant(ClaudeRefusalUnnamed) {
			res.Message = "trust = hostile: egress_allow is incomplete"
		}
	}
	return res, nil
}

// Wait passes time: a connection the proxy let through late arrives, and
// so does a stray one.
func (e *Egress) Wait(ctx context.Context, d time.Duration) error {
	err := e.Link.Wait(ctx, d)
	stray := e.fault(StrayConnection)
	e.em.Lock()
	defer e.em.Unlock()
	for _, l := range e.late {
		if !l.closed {
			l.accepted++
			l.received = append(l.received, "late")
		}
	}
	e.late = nil
	if stray && !e.stray {
		e.stray = true
		for _, l := range e.listeners {
			if !l.closed {
				l.accepted++
				l.received = append(l.received, "")
			}
		}
	}
	return err
}

// The sandbox side.

var (
	tokPat       = `(conf-[a-z0-9]+-[a-z0-9-]+)`
	reConnect    = regexp.MustCompile(`^echo '` + tokPat + `' \| caboose-agent connect '([^']+)' '([0-9]+)' 1>&2$`)
	reCurlProxy  = regexp.MustCompile(`^curl -g -sS -f --max-time 20 --noproxy '' --proxy 'http://127\.0\.0\.1:9128' 'http://([^/']+)/` + tokPat + `' 1>&2$`)
	reCurlDirect = regexp.MustCompile(`^curl -g -sS -f --max-time 10 --noproxy '([^']+)' 'http://([^/']+)/` + tokPat + `' 1>&2$`)
	rePing       = regexp.MustCompile(`^ping -c 1 -W 2 '([^']+)' 1>&2$`)
	reRoutes     = regexp.MustCompile(`^` + regexp.QuoteMeta(conformance.Routes().Body()) + `$`)
	reDevices    = regexp.MustCompile(`^` + regexp.QuoteMeta(conformance.NetDevices().Body()) + `$`)
)

func egressForm(body string) bool {
	for _, re := range []*regexp.Regexp{reConnect, reCurlProxy, reCurlDirect, rePing, reRoutes, reDevices} {
		if re.MatchString(body) {
			return true
		}
	}
	return false
}

// Exec runs the egress checks' commands in the model and hands the rest
// to the Link.
func (e *Egress) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	if rc, o, e, ok := e.agentShut(user); ok {
		return rc, o, e, nil
	}
	body, ok := conformance.Unwrap(argv)
	if !ok || !egressForm(body) {
		return e.Link.Exec(ctx, user, argv, stdin)
	}
	e.mu.Lock()
	down := e.down || e.absent || e.stopped
	shellNo := e.shellNo
	if !down && !shellNo {
		e.event("exec")
	}
	e.mu.Unlock()
	if down {
		return -1, "", "", errors.New("the sandbox is not running")
	}
	if shellNo {
		return 127, "", "exec: the shell is not there: no such file or directory", nil
	}
	rc, msg := e.run(user, body)
	return 0, fmt.Sprintf("\nrc=%d\n", rc), msg, nil
}

func (e *Egress) isolation() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.isolate
}

func (e *Egress) trustMade() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.made
}

// parseHostPort reads a URL's authority.
func parseHostPort(s string) (string, int, bool) {
	h, p, ok := strings.Cut(s, "]:")
	if ok && strings.HasPrefix(h, "[") {
		h = h[1:]
	} else if i := strings.LastIndex(s, ":"); i > 0 {
		h, p = s[:i], s[i+1:]
	} else {
		return "", 0, false
	}
	n, err := strconv.Atoi(p)
	return h, n, err == nil
}

func (e *Egress) run(user, body string) (int, string) {
	vm := e.isolation() == "vm"
	switch {
	case reConnect.MatchString(body):
		m := reConnect.FindStringSubmatch(body)
		port, _ := strconv.Atoi(m[3])
		return e.viaProxy(vm, m[2], port, m[1], true)
	case reCurlProxy.MatchString(body):
		if e.fault(CurlMissing) {
			return 127, "sh: 1: curl: not found"
		}
		m := reCurlProxy.FindStringSubmatch(body)
		h, port, ok := parseHostPort(m[1])
		if !ok {
			return 3, "curl: (3) URL rejected: Bad hostname"
		}
		return e.viaProxy(vm, h, port, m[2], false)
	case reCurlDirect.MatchString(body):
		if e.fault(CurlMissing) {
			return 127, "sh: 1: curl: not found"
		}
		m := reCurlDirect.FindStringSubmatch(body)
		h, port, ok := parseHostPort(m[2])
		if !ok || h != m[1] {
			return 3, "curl: (3) URL rejected: Bad hostname"
		}
		return e.direct(vm, h, port, m[3])
	case rePing.MatchString(body):
		if e.fault(PingMissing) {
			return 127, "sh: 1: ping: not found"
		}
		h := rePing.FindStringSubmatch(body)[1]
		e.target(h + ":icmp")
		if user != "root" {
			return 1, "ping: permission denied (are you root?)"
		}
		if !vm || e.trustMade() != conformance.TrustHostile || e.mutant(GuestNAT) || e.mutant(GuestPing) {
			return 0, "64 bytes from " + h + ": seq=0 ttl=64 time=0.5 ms"
		}
		return 1, "ping: sendto: Network unreachable"
	case reRoutes.MatchString(body):
		if e.fault(RoutesFail) {
			return 1, "ip: can't find device"
		}
		if e.fault(RoutesComplain) {
			return 0, "Error: argument \"all\" is wrong: table id value is invalid"
		}
		// The guest's own dockerd's bridge, as a vm guest has it, in the
		// main and local tables of both families.
		bridge := "172.17.0.0/16 dev docker0 proto kernel scope link src 172.17.0.1 linkdown\n" +
			"local 127.0.0.0/8 dev lo table local proto kernel scope host src 127.0.0.1\n" +
			"broadcast 172.17.255.255 dev docker0 table local proto kernel scope link src 172.17.0.1 linkdown\n" +
			"local 172.17.0.1 dev docker0 table local proto kernel scope host src 172.17.0.1\n"
		v6 := "fe80::/64 dev docker0 proto kernel metric 256 linkdown pref medium\n" +
			"local ::1 dev lo table local proto kernel metric 0 pref medium\n" +
			"multicast ff00::/8 dev docker0 table local proto kernel metric 256 linkdown pref medium\n"
		switch {
		case !vm || e.trustMade() != conformance.TrustHostile || e.mutant(GuestNAT) || e.mutant(GuestRoute):
			return 0, "default via 192.168.64.1 dev eth0\n192.168.64.0/24 dev eth0 scope link  src 192.168.64.2\n" + bridge + v6
		case e.mutant(GuestNICRoute):
			return 0, "192.168.64.0/24 dev eth0 scope link  src 192.168.64.2\n" + bridge + v6
		case e.mutant(GuestV6Default):
			return 0, bridge + "default via fe80::1 dev docker0 metric 1024 pref medium\n" + v6
		case e.mutant(GuestUnlistedRoute):
			return 0, bridge + "10.8.0.0/24 dev tun0 scope link\n" + v6
		}
		return 0, bridge + v6
	case reDevices.MatchString(body):
		if e.fault(DevicesFail) {
			return 1, ""
		}
		hostile := vm && e.trustMade() == conformance.TrustHostile
		switch {
		case !hostile || e.mutant(GuestNAT) || e.mutant(GuestRoute) || e.mutant(GuestNICRoute) || e.mutant(GuestNICBesideBridge):
			return 0, "virtual docker0\nnic eth0\nvirtual lo\nvirtio-net virtio0"
		case e.mutant(GuestNICInOtherNamespace):
			return 0, "virtual docker0\nvirtual lo\nvirtio-net virtio0"
		}
		return 0, "virtual docker0\nvirtual lo"
	}
	return 127, "sh: not a form the model knows"
}

func (e *Egress) target(s string) {
	e.em.Lock()
	defer e.em.Unlock()
	e.targets = append(e.targets, s)
}

// arrive connects to the listener at a:port, if one is open: whether it
// was there.
func (e *Egress) arrive(a netip.Addr, port int, token string) bool {
	e.em.Lock()
	defer e.em.Unlock()
	for _, l := range e.listeners {
		if l.addr == a && l.port == port && !l.closed {
			l.accepted++
			l.received = append(l.received, token)
			return true
		}
	}
	return false
}

func (e *Egress) arriveLate(a netip.Addr, port int) {
	e.em.Lock()
	defer e.em.Unlock()
	for _, l := range e.listeners {
		if l.addr == a && l.port == port && !l.closed {
			e.late = append(e.late, l)
		}
	}
}

const modelReply = "caboose-conformance 0a1b2c3d-fake-nonce"

// viaProxy is a connection through the outbound proxy: the host's
// decision, as the product makes it, and what each client prints.
func (e *Egress) viaProxy(vm bool, host string, port int, token string, connect bool) (int, string) {
	e.target(hostPortOf(host, port))
	if !vm || e.fault(ProxyDown) {
		if connect {
			return 1, "caboose-agent: caboose's outbound proxy is off: no host is linked to this sandbox right now"
		}
		return 7, "curl: (7) Failed to connect to 127.0.0.1 port 9128 after 0 ms: Couldn't connect to server"
	}
	target := hostPortOf(host, port)
	addr, verdict, why := e.decide(host, port)
	answer := func(status int, msg string) (int, string) {
		if connect {
			return 1, fmt.Sprintf("caboose-agent: cannot connect to %s through the host (%d): %s", target, status, msg)
		}
		return 22, fmt.Sprintf("curl: (22) The requested URL returned error: %d", status)
	}
	if verdict != "allow" {
		if e.mutant(ProxyLies) && addr.IsValid() {
			e.arrive(addr, port, token)
		}
		if e.mutant(ProxyLate) {
			e.arriveLate(addr, port)
		}
		if e.fault(RefusalUnnamed) {
			why = "refused"
		}
		if e.fault(SandboxClaimsConnect) {
			return 0, modelReply
		}
		return answer(403, why)
	}
	sent := token
	if !connect {
		sent = "GET /" + token + " HTTP/1.1"
	}
	if !e.arrive(addr, port, sent) {
		return answer(502, target+": connection refused")
	}
	if connect {
		return 0, modelReply
	}
	return answer(502, "no HTTP response from "+target)
}

func hostPortOf(host string, port int) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

func (e *Egress) decide(host string, port int) (netip.Addr, string, string) {
	e.em.Lock()
	p := clonePolicy(e.policy)
	e.em.Unlock()
	target := hostPortOf(host, port)
	literal := true
	a, err := netip.ParseAddr(host)
	if err != nil {
		literal = false
		if host != "localhost" {
			return netip.Addr{}, "deny", target + ": does not resolve"
		}
		a = modelLoopback
	}
	if e.mutant(EgressOpen) {
		return a, "allow", ""
	}
	allowIgnored := e.fault(AllowIgnored)
	names := func(list []string) bool { return !literal && slices.Contains(list, host) }
	addrIn := func(list []string, exactOnly bool) (exact, wide bool) {
		for _, s := range list {
			if b, err := netip.ParseAddr(s); err == nil && b == a {
				exact = true
			}
			if pf, err := netip.ParsePrefix(s); err == nil && pf.Contains(a) && !exactOnly {
				wide = true
			}
		}
		return exact, wide
	}
	ports := p.Ports
	if ports == nil {
		ports = []int{22, 80, 443}
	}
	if !slices.Contains(ports, port) && !e.mutant(PortsIgnored) {
		return a, "deny", fmt.Sprintf("%s: port %d is not in egress_ports (config.toml on the host)", target, port)
	}
	allowed := func() bool {
		if allowIgnored {
			return false
		}
		ex, wide := addrIn(p.Allow, false)
		return names(p.Allow) || ex || wide
	}
	if dx, dw := addrIn(p.Deny, false); (names(p.Deny) || dx || dw) && !(e.mutant(AllowBeatsDeny) && allowed()) {
		return a, "deny", target + ": egress_deny (config.toml on the host) names it"
	}
	if e.mutant(UnnamedAllowed) {
		return a, "allow", ""
	}
	// This machine's own: loopback, and its LAN address.
	if a == modelLoopback || a == modelLAN {
		ex, wide := addrIn(p.Allow, false)
		if allowIgnored {
			ex, wide = false, false
		}
		inRange := ex
		if a == modelLoopback {
			// An address or network within 127.0.0.0/8 lets it through.
			for _, s := range p.Allow {
				if pf, err := netip.ParsePrefix(s); err == nil && pf.Bits() >= 8 && pf.Contains(a) && netip.MustParsePrefix("127.0.0.0/8").Contains(pf.Addr()) {
					inRange = !allowIgnored
				}
			}
		}
		switch {
		case inRange:
		case a == modelLoopback && names(p.Allow) && e.mutant(LoopbackByName) && !allowIgnored:
		case a == modelLAN && wide && e.mutant(OwnByNetwork):
		default:
			return a, "deny", fmt.Sprintf("%s: %s is a loopback, unspecified or link-local address, or one of the host's own, which only an egress_allow address or CIDR within its range lets through", target, a)
		}
		return a, "allow", ""
	}
	if allowed() {
		return a, "allow", ""
	}
	if a.IsPrivate() || e.trustMade() == conformance.TrustHostile {
		return a, "pending", fmt.Sprintf("%s: refused for now, pending #1: not named by egress_allow", target)
	}
	return a, "allow", ""
}

// direct is a connection around the proxy, from the guest's own network.
func (e *Egress) direct(vm bool, host string, port int, token string) (int, string) {
	e.target(hostPortOf(host, port))
	a, err := netip.ParseAddr(host)
	if err != nil {
		return 6, "curl: (6) Could not resolve host: " + host
	}
	reach := true
	switch {
	case !vm:
		reach = !e.fault(ContainerBlocked)
	case e.fault(NATBlocked):
		reach = false
	case e.trustMade() == conformance.TrustHostile:
		reach = e.mutant(GuestNAT)
		if !reach {
			return 7, "curl: (7) Failed to connect to " + host + " port " + strconv.Itoa(port) + " after 0 ms: Network is unreachable"
		}
	}
	if !reach || a == modelDoc {
		return 28, "curl: (28) Connection timed out after 10001 milliseconds"
	}
	if !e.arrive(a, port, "GET /"+token+" HTTP/1.1") {
		return 7, "curl: (7) Failed to connect: Connection refused"
	}
	return 1, "curl: (1) Received HTTP/0.9 when not allowed"
}
