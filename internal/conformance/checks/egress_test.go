package checks

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/fake"
)

// The worlds the egress checks run in, and what each check comes to there
// when every control holds.
var egressWorlds = []struct {
	hostWorld
	want map[string]c.Status
}{
	{hostWorld{"vm", c.TrustFallible}, map[string]c.Status{"E1": c.Pass, "E2": c.Pass, "E3": c.Pass, "E4": c.Gap, "E5": c.Skip, "E6": c.Pass}},
	{hostWorld{"vm", c.TrustHostile}, map[string]c.Status{"E1": c.Pass, "E2": c.Pass, "E3": c.Pass, "E4": c.Pass, "E5": c.Skip, "E6": c.Pass, "E7": c.Pass}},
	{hostWorld{"container", c.TrustFallible}, map[string]c.Status{"E8": c.Gap}},
	{hostWorld{"gvisor", c.TrustFallible}, map[string]c.Status{"E8": c.Gap}},
	{hostWorld{"container", c.TrustHostile}, map[string]c.Status{}},
	{hostWorld{"gvisor", c.TrustHostile}, map[string]c.Status{}},
}

func newEgress(t testing.TB, w hostWorld) *fake.Egress {
	t.Helper()
	e, err := fake.NewEgress(hostN8, w.iso, w.trust, claudeHostNames)
	if err != nil {
		t.Fatal(err)
	}
	fake.NoAgentRuns(t, e.Fake)
	return e
}

func egressApplicable(w hostWorld) []c.Check {
	var out []c.Check
	for _, ck := range EgressChecks() {
		if ck.Applies(w.iso, w.trust) {
			out = append(out, ck)
		}
	}
	return out
}

// egressOne runs one check alone in a fresh model of w, set up by set.
func egressOne(t testing.TB, w hostWorld, ck c.Check, set func(*fake.Egress)) (c.Result, *fake.Egress) {
	t.Helper()
	e := newEgress(t, w)
	if set != nil {
		set(e)
	}
	return hostRunIn(t, e.Link, e.Env(w.iso, w.trust), []c.Check{ck})[ck.ID], e
}

// afterRun holds a model to what every egress check leaves: its
// listeners closed, the egress keys as they were, the envelope's order,
// and no notification past the stubs.
func afterRun(t *testing.T, label string, e *fake.Egress, before c.EgressPolicy) {
	t.Helper()
	if n := e.OpenListeners(); n != 0 {
		t.Errorf("%s: %d listener(s) left open", label, n)
	}
	if now := e.Policy(); !now.Equal(before) {
		t.Errorf("%s: egress keys left as %+v, were %+v", label, now, before)
	}
	if err := fake.CheckOrder(e.Events()); err != nil {
		t.Errorf("%s: %v", label, err)
	}
	if e.Leaked() != 0 {
		t.Errorf("%s: a notification reached no stub", label)
	}
}

func TestEgressEnforcing(t *testing.T) {
	for _, w := range egressWorlds {
		checks := egressApplicable(w.hostWorld)
		if len(checks) != len(w.want) {
			t.Errorf("%v: %d checks apply, want %d", w.hostWorld, len(checks), len(w.want))
		}
		e := newEgress(t, w.hostWorld)
		before := e.Policy()
		got := hostRunIn(t, e.Link, e.Env(w.iso, w.trust), checks)
		for id, want := range w.want {
			if r := got[id]; r.Status != want {
				t.Errorf("%v %s: %s %s, want %s", w.hostWorld, id, r.Status, r.Reason, want)
			}
		}
		afterRun(t, w.iso+"/"+w.trust, e, before)
	}
}

func TestEgressChecksList(t *testing.T) {
	seen := map[string]bool{}
	for _, ck := range EgressChecks() {
		if seen[ck.ID] {
			t.Errorf("%s twice", ck.ID)
		}
		seen[ck.ID] = true
		if ck.Destructive() || ck.Wipe {
			t.Errorf("%s is destructive", ck.ID)
		}
		if ck.Group != groupEgress || ck.Control == "" || ck.Title == "" {
			t.Errorf("%s: group %q, control %q", ck.ID, ck.Group, ck.Control)
		}
	}
	for i := 1; i <= 8; i++ {
		if !seen["E"+strconv.Itoa(i)] {
			t.Errorf("no E%d", i)
		}
	}
	for _, ck := range EgressChecks() {
		if ck.ID != "E8" && ck.Applies("container", c.TrustFallible) {
			t.Errorf("%s runs under container", ck.ID)
		}
		if ck.ID == "E8" && (ck.Applies("vm", c.TrustFallible) || ck.Applies("container", c.TrustHostile)) {
			t.Errorf("E8 runs under vm or hostile")
		}
	}
}

// egressTable is which checks each mutant must fail, in the worlds where
// they run.
var egressTable = map[fake.Mutant][]string{
	fake.EgressOpen:               {"E1", "E2", "E3", "E6"},
	fake.LoopbackByName:           {"E1"},
	fake.OwnByNetwork:             {"E1"},
	fake.UnnamedAllowed:           {"E1", "E2", "E3"},
	fake.PortsIgnored:             {"E3"},
	fake.AllowBeatsDeny:           {"E6"},
	fake.ProxyLies:                {"E1", "E2", "E3", "E6"},
	fake.ProxyLate:                {"E1", "E2", "E3", "E6"},
	fake.GuestNAT:                 {"E4"},
	fake.GuestRoute:               {"E4"},
	fake.GuestNICRoute:            {"E4"},
	fake.GuestNICBesideBridge:     {"E4"},
	fake.GuestNICInOtherNamespace: {"E4"},
	fake.GuestV6Default:           {"E4"},
	fake.GuestUnlistedRoute:       {"E4"},
	fake.GuestPing:                {"E4"},
	fake.ClaudeHostsUnchecked:     {"E7"},
	fake.ClaudeDenyIgnored:        {"E7"},
	fake.ClaudeRefusalUnnamed:     {"E7"},
	fake.DecisionCreates:          {"E7"},
}

// TestEgressMutants holds every check to its mutants, each check alone in
// a fresh model: a mutant fails the checks it is assigned in the worlds
// where they run, and leaves every other as it is enforcing; every mutant
// fails something, and every check that passes is failed by a mutant.
func TestEgressMutants(t *testing.T) {
	for _, m := range fake.EgressMutants {
		if _, ok := egressTable[m]; !ok {
			t.Errorf("mutant %q has no row", m)
		}
	}
	failed := map[fake.Mutant]bool{}
	covered := map[string]bool{}
	for _, w := range egressWorlds {
		for _, ck := range egressApplicable(w.hostWorld) {
			for _, m := range fake.EgressMutants {
				want := w.want[ck.ID]
				if slices.Contains(egressTable[m], ck.ID) && want == c.Pass {
					want = c.Fail
				}
				got, e := egressOne(t, w.hostWorld, ck, func(e *fake.Egress) { e.Break(m) })
				if got.Status != want {
					t.Errorf("%v %s under %q: %s (%s), want %s", w.hostWorld, ck.ID, m, got.Status, got.Reason, want)
				}
				if got.Status == c.Fail {
					failed[m] = true
					covered[ck.ID] = true
				}
				if n := e.OpenListeners(); n != 0 {
					t.Errorf("%v %s under %q: %d listener(s) left open", w.hostWorld, ck.ID, m, n)
				}
			}
		}
	}
	for _, m := range fake.EgressMutants {
		if !failed[m] {
			t.Errorf("mutant %q fails nothing", m)
		}
	}
	for _, id := range []string{"E1", "E2", "E3", "E4", "E6", "E7"} {
		if !covered[id] {
			t.Errorf("%s is failed by no mutant", id)
		}
	}
}

// egressFaults are what each fault makes of the checks, in every world
// where they run; a check it does not name is as it is enforcing. A
// positive control that fails, a refusal the check was not written for or
// a setup gone wrong is an ERROR, a missing LAN address a SKIP, and a
// connection the check did not make a FAIL.
var egressFaults = map[fake.Fault]map[string]c.Status{
	fake.ProxyDown:         {"E1": c.Error, "E2": c.Error, "E3": c.Error, "E4/hostile": c.Error, "E6": c.Error},
	fake.AllowIgnored:      {"E1": c.Error, "E2": c.Error, "E3": c.Error, "E4/hostile": c.Error, "E6": c.Error},
	fake.SetEgressFails:    {"E1": c.Error, "E2": c.Error, "E3": c.Error, "E4/hostile": c.Error, "E6": c.Error},
	fake.RestoreFails:      {"E1": c.Error, "E2": c.Error, "E3": c.Error, "E4/hostile": c.Error, "E6": c.Error},
	fake.EgressUnreadable:  {"E1": c.Error, "E2": c.Error, "E3": c.Error, "E4": c.Error, "E6": c.Error, "E7": c.Error, "E8": c.Error},
	fake.ListenFails:       {"E1": c.Error, "E2": c.Error, "E3": c.Error, "E4": c.Error, "E6": c.Error, "E8": c.Error},
	fake.NoLAN:             {"E2": c.Skip, "E3": c.Skip, "E4": c.Skip, "E6": c.Skip, "E8": c.Skip},
	fake.CurlMissing:       {"E1": c.Error, "E2": c.Error, "E3": c.Error, "E4": c.Error, "E6": c.Error, "E8": c.Error},
	fake.PingMissing:       {"E4/hostile": c.Error},
	fake.RoutesFail:        {"E4/hostile": c.Error},
	fake.DevicesFail:       {"E4/hostile": c.Error},
	fake.RoutesComplain:    {"E4/hostile": c.Error},
	fake.RefusalUnnamed:    {"E3": c.Error, "E6": c.Error},
	fake.ClaudeRefused:     {"E7": c.Error},
	fake.LaunchFails:       {"E7": c.Error},
	fake.StrayConnection:   {"E1": c.Fail, "E2": c.Fail, "E3": c.Fail, "E4/hostile": c.Fail, "E6": c.Fail},
	fake.ContainerBlocked:  {},
	fake.LauncherNeedsMore: {"E7": c.Error},
	// A connection the sandbox reports and no listener saw is a bypass to
	// something else.
	fake.SandboxClaimsConnect: {"E1": c.Fail, "E2": c.Fail, "E3": c.Fail, "E6": c.Fail},
	fake.NATBlocked:           {},
}

func TestEgressFaults(t *testing.T) {
	for _, f := range fake.EgressFaults {
		if _, ok := egressFaults[f]; !ok {
			t.Errorf("fault %q has no row", f)
		}
	}
	for _, w := range egressWorlds {
		for _, ck := range egressApplicable(w.hostWorld) {
			for _, f := range fake.EgressFaults {
				want, ok := egressFaults[f][ck.ID+"/"+w.trust]
				if !ok {
					want, ok = egressFaults[f][ck.ID]
				}
				if !ok {
					want = w.want[ck.ID]
				}
				got, e := egressOne(t, w.hostWorld, ck, func(e *fake.Egress) { e.SetFault(f) })
				if got.Status != want {
					t.Errorf("%v %s with %q: %s (%s), want %s", w.hostWorld, ck.ID, f, got.Status, got.Reason, want)
				}
				if n := e.OpenListeners(); n != 0 {
					t.Errorf("%v %s with %q: %d listener(s) left open", w.hostWorld, ck.ID, f, n)
				}
			}
		}
	}
}

// E1 without a LAN address tries loopback alone, and says what it left.
func TestE1WithoutLAN(t *testing.T) {
	w := hostWorld{"vm", c.TrustHostile}
	e := newEgress(t, w)
	e.SetFault(fake.NoLAN)
	r := &c.Runner{Env: &fake.Envelope{F: e.Fake}}
	res, col, err := c.RunChecks(context.Background(), r, []c.Check{e1()}, e.Env(w.iso, w.trust))
	if err != nil || len(res) != 1 || res[0].Status != c.Pass {
		t.Fatalf("E1 without LAN: %v %v", res, err)
	}
	if !slices.ContainsFunc(col.Lines("E1"), func(l string) bool { return strings.Contains(l, "not tried") }) {
		t.Errorf("E1 without LAN does not say what it did not try: %q", col.Lines("E1"))
	}
	if len(e.Listeners()) != 1 {
		t.Errorf("E1 without LAN started %d listeners", len(e.Listeners()))
	}
}

// Every check that sends anything needs a running sandbox, its shell and
// the host's calls: without them it is an ERROR, never a PASS.
func TestEgressNoVacuousPass(t *testing.T) {
	faults := []struct {
		name string
		set  func(*fake.Egress)
	}{
		{"sandbox down", func(e *fake.Egress) { e.SetDown(true) }},
		{"shell missing", func(e *fake.Egress) { e.SetShellMissing(true) }},
		{"stubs missing", func(e *fake.Egress) { e.SetStubsMissing(true) }},
	}
	for _, w := range egressWorlds {
		for _, ck := range egressApplicable(w.hostWorld) {
			if ck.ID == "E5" || ck.ID == "E7" {
				continue // E5 sends nothing; E7 asks the launcher alone
			}
			for _, f := range faults {
				got, e := egressOne(t, w.hostWorld, ck, f.set)
				if got.Status != c.Error {
					t.Errorf("%v %s with %s: %s (%s), want ERROR", w.hostWorld, ck.ID, f.name, got.Status, got.Reason)
				}
				if f.name == "stubs missing" {
					for _, ev := range e.Events() {
						if strings.HasPrefix(ev, "exec") {
							t.Errorf("%v %s without stubs sent a command", w.hostWorld, ck.ID)
							break
						}
					}
				}
			}
			// A host without the egress calls, or without any.
			e := newEgress(t, w.hostWorld)
			for name, env := range map[string]c.Env{
				"no egress calls": c.NewEnv(e, e.Link, e, w.iso, w.trust),
				"no host calls":   c.NewEnv(e, e.Fake, e, w.iso, w.trust),
			} {
				if got := hostRunIn(t, e.Link, env, []c.Check{ck})[ck.ID]; got.Status != c.Error {
					t.Errorf("%v %s with %s: %s (%s), want ERROR", w.hostWorld, ck.ID, name, got.Status, got.Reason)
				}
			}
		}
	}
}

// E4 under fallible is a GAP whether or not the guest gets through, and
// says which.
func TestE4FallibleReasons(t *testing.T) {
	w := hostWorld{"vm", c.TrustFallible}
	got, _ := egressOne(t, w, e4(), nil)
	if got.Status != c.Gap || !strings.Contains(got.Reason, "reaches") {
		t.Errorf("E4 fallible: %s %q", got.Status, got.Reason)
	}
	got, _ = egressOne(t, w, e4(), func(e *fake.Egress) { e.SetFault(fake.NATBlocked) })
	if got.Status != c.Gap || !strings.Contains(got.Reason, "did not reach") {
		t.Errorf("E4 fallible, NAT blocked: %s %q", got.Status, got.Reason)
	}
}

func TestE8Reasons(t *testing.T) {
	for _, iso := range []string{"container", "gvisor"} {
		w := hostWorld{iso, c.TrustFallible}
		got, _ := egressOne(t, w, e8(), nil)
		if got.Status != c.Gap || got.Reason != gapOpen {
			t.Errorf("%s: E8 %s %q, want GAP %q", iso, got.Status, got.Reason, gapOpen)
		}
		got, _ = egressOne(t, w, e8(), func(e *fake.Egress) { e.SetFault(fake.ContainerBlocked) })
		if got.Status != c.Gap || got.Reason != gapClosed {
			t.Errorf("%s: E8 blocked %s %q, want GAP %q", iso, got.Status, got.Reason, gapClosed)
		}
	}
}

// egressRecording keeps every command an Egress is sent.
type egressRecording struct {
	*fake.Egress
	mu   sync.Mutex
	sent []string
}

func (r *egressRecording) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	if body, ok := c.Unwrap(argv); ok {
		r.mu.Lock()
		r.sent = append(r.sent, body)
		r.mu.Unlock()
	}
	return r.Egress.Exec(ctx, user, argv, stdin)
}

var egressForm = regexp.MustCompile(`^(echo 'conf-e[0-9]-[0-9]+' \| caboose-agent connect '([0-9a-f.:]+|localhost)' '[0-9]+' 1>&2` +
	`|curl -g -sS -f --max-time 20 --noproxy '' --proxy 'http://127\.0\.0\.1:9128' 'http://[0-9a-z.:\[\]]+/conf-e[0-9]-[0-9]+' 1>&2` +
	`|curl -g -sS -f --max-time 10 --noproxy '[0-9a-f.:]+' 'http://[0-9a-f.:\[\]]+/conf-e[0-9]-[0-9]+' 1>&2` +
	`|ping -c 1 -W 2 '[0-9a-f.:]+' 1>&2` +
	`|ip route show 1>&2)$`)

// Every connection a check makes is to one of the suite's own listeners,
// by its address (or the name localhost, for the loopback one), or to the
// documentation address at a listener's port (E4 under hostile alone);
// every command is literal and valid sh. That holds with every mutant on,
// too.
func TestEgressTargetsAreTheSuites(t *testing.T) {
	sent := map[string]bool{}
	for _, w := range egressWorlds {
		for _, m := range append([]fake.Mutant{fake.Enforcing}, fake.EgressMutants...) {
			for _, ck := range egressApplicable(w.hostWorld) {
				e := newEgress(t, w.hostWorld)
				e.Break(m)
				r := &egressRecording{Egress: e}
				hostRunIn(t, e.Link, c.NewEnv(r, r, r, w.iso, w.trust), []c.Check{ck})
				allowed := map[string]bool{}
				var ports []int
				for _, l := range e.Listeners() {
					allowed[l.Addr()] = true
					ap := netip.MustParseAddrPort(l.Addr())
					ports = append(ports, int(ap.Port()))
					if ap.Addr().IsLoopback() {
						allowed["localhost:"+strconv.Itoa(int(ap.Port()))] = true
					}
					allowed[ap.Addr().String()+":icmp"] = true
				}
				for _, tg := range e.Targets() {
					if allowed[tg] {
						continue
					}
					doc := false
					for _, p := range ports {
						doc = doc || tg == "192.0.2.1:"+strconv.Itoa(p)
					}
					if !doc || ck.ID != "E4" || w.trust != c.TrustHostile {
						t.Errorf("%v %s under %q: connected to %s, not a listener of the suite's", w.hostWorld, ck.ID, m, tg)
					}
				}
				for _, b := range r.sent {
					sent[b] = true
				}
			}
		}
	}
	if len(sent) < 10 {
		t.Fatalf("only %d distinct commands", len(sent))
	}
	for b := range sent {
		// The interfaces' list and the routes are literals, taking
		// nothing from a check: their globs and expansions name /sys and
		// /proc alone.
		fixed := b == c.NetDevices().Body() || b == c.Routes().Body()
		if !egressForm.MatchString(b) && !fixed {
			t.Errorf("not a form the egress checks send: %q", b)
		}
		if strings.ContainsAny(b, "$`~*?") && !fixed {
			t.Errorf("an expansion, a backtick, a tilde or a glob: %q", b)
		}
		standIn := strings.ReplaceAll(b, "caboose-agent", "conf-agent-stand-in")
		if out, err := exec.Command("sh", "-n", "-c", "LC_ALL=C; { "+standIn+"; } >/dev/null").CombinedOutput(); err != nil {
			t.Errorf("sh -n rejected %q: %v %s", standIn, err, out)
		}
	}
}

// A check that is handed a listener the suite did not start, on a public
// address, sends nothing to it.
type foreignListener struct{ c.SuiteListener }

func (foreignListener) Addr() string { return "203.0.113.7:443" }

type foreignEgress struct{ *fake.Egress }

func (f foreignEgress) StartListener(ctx context.Context, s c.Scope) (c.SuiteListener, error) {
	l, err := f.Egress.StartListener(ctx, s)
	if err != nil {
		return nil, err
	}
	return foreignListener{l}, nil
}

func TestEgressRefusesAForeignListener(t *testing.T) {
	for _, w := range egressWorlds {
		for _, ck := range egressApplicable(w.hostWorld) {
			if ck.ID == "E5" || ck.ID == "E7" {
				continue
			}
			e := newEgress(t, w.hostWorld)
			f := foreignEgress{e}
			got := hostRunIn(t, e.Link, c.NewEnv(e, f, e, w.iso, w.trust), []c.Check{ck})[ck.ID]
			if got.Status != c.Error {
				t.Errorf("%v %s with a foreign listener: %s (%s), want ERROR", w.hostWorld, ck.ID, got.Status, got.Reason)
			}
			if len(e.Targets()) != 0 {
				t.Errorf("%v %s with a foreign listener connected to %v", w.hostWorld, ck.ID, e.Targets())
			}
			if n := e.OpenListeners(); n != 0 {
				t.Errorf("%v %s: %d listener(s) left open", w.hostWorld, ck.ID, n)
			}
		}
	}
}

// The egress check file runs nothing itself and names no address: every
// address it reaches comes from a listener of the suite's (or is the
// documentation address, which conformance names).
func TestEgressSourceIsInert(t *testing.T) {
	banned := map[string]bool{"os": true, "os/exec": true, "net": true, "net/http": true, "syscall": true, "golang.org/x/sys/unix": true, "unsafe": true}
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, "egress.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		if banned[p] {
			t.Errorf("egress.go imports %s", p)
		}
	}
	hostPort := regexp.MustCompile(`[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|[0-9a-f]*::[0-9a-f]*|:[0-9]{2,5}\b`)
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, _ := strconv.Unquote(lit.Value)
		line := fs.Position(lit.Pos()).Line
		if hostPort.MatchString(s) {
			t.Errorf("egress.go:%d: an address or port in a string: %q", line, s)
		}
		if regexp.MustCompile(`^(curl -|ping -c|ip route show|echo '|caboose-agent [a-z-]+ ')`).MatchString(s) {
			t.Errorf("egress.go:%d: a command outside the builders: %q", line, s)
		}
		for _, bad := range []string{".ssh", ".credentials", "ssh_auth_sock", "/users/", "$home", "keychain"} {
			if strings.Contains(strings.ToLower(s), bad) {
				t.Errorf("egress.go:%d: a string naming %q", line, bad)
			}
		}
		return true
	})
}
