package checks

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
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

// hostWorld is one isolation and trust the host and trust checks are run
// under.
type hostWorld struct{ iso, trust string }

var hostWorlds = []hostWorld{
	{"container", c.TrustFallible}, {"gvisor", c.TrustFallible}, {"vm", c.TrustFallible},
	{"vm", c.TrustHostile}, {"container", c.TrustHostile}, {"gvisor", c.TrustHostile},
}

const hostN8 = "0a1b2c3d"

func newLink(t testing.TB, w hostWorld) *fake.Link {
	t.Helper()
	l, err := fake.NewLink(hostN8, w.iso, w.trust)
	if err != nil {
		t.Fatal(err)
	}
	fake.NoAgentRuns(t, l.Fake)
	return l
}

// hostRunIn runs the checks in a Link of world w, through the runner and
// its envelope, and returns each check's result by id.
func hostRunIn(t testing.TB, l *fake.Link, env c.Env, checks []c.Check) map[string]c.Result {
	t.Helper()
	r := &c.Runner{Env: &fake.Envelope{F: l.Fake}}
	res, _, err := c.RunChecks(context.Background(), r, checks, env)
	if err != nil {
		var te *c.TouchedError
		if !errors.As(err, &te) {
			t.Fatalf("run: %v", err)
		}
		t.Errorf("run touched what it was not handed: %v", err)
	}
	out := map[string]c.Result{}
	for _, x := range res {
		out[x.ID] = x
	}
	return out
}

// hostOne runs one check alone in a fresh Link of w under mutant m.
func hostOne(t testing.TB, w hostWorld, ck c.Check, m fake.Mutant) c.Result {
	t.Helper()
	l := newLink(t, w)
	l.Break(m)
	return hostRunIn(t, l, l.Env(w.iso, w.trust), []c.Check{ck})[ck.ID]
}

func TestHostChecksEnforcing(t *testing.T) {
	for _, w := range hostWorlds {
		checks := ForRun(w.iso, w.trust)
		if len(checks) == 0 {
			t.Fatalf("%v: no checks", w)
		}
		l := newLink(t, w)
		got := hostRunIn(t, l, l.Env(w.iso, w.trust), checks)
		for _, ck := range checks {
			r, ok := got[ck.ID]
			want := c.Pass
			if !ok || r.Status != want {
				t.Errorf("%v %s: %s %s, want %s", w, ck.ID, r.Status, r.Reason, want)
			}
		}
		if n := l.Leaked(); n != 0 {
			t.Errorf("%v: %d notifications reached no stub", w, n)
		}
		if n := l.Claude(); n != 0 {
			t.Errorf("%v: %d Claude Code processes started", w, n)
		}
		if err := fake.CheckOrder(l.Events()); err != nil {
			t.Errorf("%v: %v", w, err)
		}
	}
}

func TestForRun(t *testing.T) {
	ids := func(cs []c.Check) []string {
		var out []string
		for _, ck := range cs {
			out = append(out, ck.ID)
		}
		return out
	}
	for _, iso := range []string{"container", "gvisor"} {
		if got := ids(ForRun(iso, c.TrustHostile)); !slices.Equal(got, []string{"T1"}) {
			t.Errorf("hostile under %s runs %v, want T1 alone", iso, got)
		}
		got := ids(ForRun(iso, c.TrustFallible))
		for _, id := range []string{"H7", "H8", "T1", "T3", "T6"} {
			if slices.Contains(got, id) {
				t.Errorf("fallible under %s runs %s", iso, id)
			}
		}
	}
	got := ids(ForRun("vm", c.TrustHostile))
	for _, id := range []string{"H1", "H7", "H8", "H10", "T3", "T4", "T5", "T6"} {
		if !slices.Contains(got, id) {
			t.Errorf("hostile under vm does not run %s", id)
		}
	}
	if slices.Contains(got, "T1") || slices.Contains(got, "T2") {
		t.Errorf("hostile under vm runs T1 or T2: %v", got)
	}
	seen := map[string]bool{}
	for _, ck := range append(HostChecks(), TrustChecks()...) {
		if seen[ck.ID] {
			t.Errorf("%s twice", ck.ID)
		}
		seen[ck.ID] = true
		if ck.Destructive() {
			t.Errorf("%s is destructive", ck.ID)
		}
	}
	if len(seen) != 16 {
		t.Errorf("%d checks, want H1-H10 and T1-T6", len(seen))
	}
}

// hostTable is which checks each mutant must fail, in the worlds where
// they run.
var hostTable = map[fake.Mutant][]string{
	fake.HostExecOn:            {"H1", "H8"},
	fake.UnknownAliasRuns:      {"H2", "H4", "H5"},
	fake.AliasArgsUnchecked:    {"H3"},
	fake.SandboxDefinesAlias:   {"H4"},
	fake.ProposalApplied:       {"H5"},
	fake.AliasRunsWithoutGrant: {"H6"},
	fake.HostileWritableRuns:   {"H7", "T3"},
	fake.HostileOffersHostExec: {"H8"},
	fake.AliasEnvLeaks:         {"H9"},
	fake.RemainsSurvive:        {"H10"},
	fake.HostileOnContainer:    {"T1"},
	fake.AttachUnchecked:       {"T2"},
	fake.AttachNoAdvice:        {"T2"},
	fake.TrustFromSandbox:      {"T3"},
	fake.TrustLabelWrong:       {"T4"},
	fake.HostileEgressOffLoads: {"T5"},
	fake.LaunchStartsClaude:    {"T1", "T2"},
	fake.RefusedAliasesRun:     {"H2", "H3", "H4", "H5", "H6", "H7", "T3"},
	fake.RefusedHostExecRuns:   {"H1"},
	fake.RefusalsExitOdd:       {"H2", "H3", "H4", "H5", "H7", "T3"},
	fake.HostRefusalExitsOdd:   {"H1", "H8"},
	fake.SandboxWritesConfig:   {"H4", "H5", "T3"},
	fake.AskRunsLater:          {"H6"},
	fake.AttachRelabels:        {"T2"},
	fake.HostileForwardsAgent:  {"T6"},
}

// hostFaults are what each fault must make of the checks: an answer the
// check was not written for is an ERROR (a created sandbox reported as
// not created is a FAIL, from the backend's state).
var hostFaults = map[fake.Fault]map[string]c.Status{
	fake.UsageUnprefixed:      {"H2": c.Error, "H3": c.Error},
	fake.HostRefusalUnnamed:   {"H1": c.Error, "H8": c.Error},
	fake.AskDenied:            {"H6": c.Error},
	fake.TrustUnnamed:         {"H7": c.Error, "T3": c.Error},
	fake.AttachAlwaysRefused:  {"T2": c.Error},
	fake.ConfigNeverLoads:     {"T5": c.Error},
	fake.HostExecNeverOffered: {"H8": c.Error},
	fake.NoAliasesListed:      {"H4": c.Error},
	fake.EnvWithoutPath:       {"H9": c.Error},
	fake.AgentNeverCarried:    {"T6": c.Error},
}

// T1 decides from the backend's state: a sandbox the launcher created and
// says it did not is still a FAIL.
func TestT1ReadsTheBackend(t *testing.T) {
	for _, iso := range []string{"container", "gvisor"} {
		w := hostWorld{iso, c.TrustHostile}
		l := newLink(t, w)
		l.Break(fake.HostileOnContainer)
		l.SetFault(fake.CreatedUnreported)
		got := hostRunIn(t, l, l.Env(w.iso, w.trust), []c.Check{t1()})["T1"]
		if got.Status != c.Fail || !strings.Contains(got.Reason, "created") {
			t.Errorf("%s: T1 with a creation unreported: %s %s, want FAIL", iso, got.Status, got.Reason)
		}
	}
}

func TestHostFaults(t *testing.T) {
	for _, f := range fake.Faults {
		if _, ok := hostFaults[f]; !ok && f != fake.CreatedUnreported {
			t.Errorf("fault %q has no row", f)
		}
	}
	hit := map[fake.Fault]bool{}
	for _, w := range hostWorlds {
		for _, ck := range ForRun(w.iso, w.trust) {
			base := hostOne(t, w, ck, fake.Enforcing).Status
			for _, f := range fake.Faults {
				want, ok := hostFaults[f][ck.ID]
				if !ok {
					want = base
				} else {
					hit[f] = true
				}
				l := newLink(t, w)
				l.SetFault(f)
				got := hostRunIn(t, l, l.Env(w.iso, w.trust), []c.Check{ck})[ck.ID]
				if got.Status != want {
					t.Errorf("%v %s with %q: %s (%s), want %s", w, ck.ID, f, got.Status, got.Reason, want)
				}
			}
		}
	}
	for _, f := range fake.Faults {
		if !hit[f] && f != fake.CreatedUnreported {
			t.Errorf("fault %q is tried by no check", f)
		}
	}
}

// TestHostMutants holds every check to its mutants, each check alone in
// a fresh model: a mutant fails the checks it is assigned that run in the
// world, and leaves every other check as it is enforcing; every mutant
// fails something somewhere, and every check that passes is failed by a
// mutant.
func TestHostMutants(t *testing.T) {
	for _, m := range fake.HostMutants {
		if _, ok := hostTable[m]; !ok {
			t.Errorf("mutant %q has no row", m)
		}
	}
	failedSomewhere := map[fake.Mutant]bool{}
	covered := map[string]bool{}
	for _, w := range hostWorlds {
		checks := ForRun(w.iso, w.trust)
		base := map[string]c.Status{}
		for _, ck := range checks {
			base[ck.ID] = hostOne(t, w, ck, fake.Enforcing).Status
		}
		for _, m := range fake.HostMutants {
			for _, ck := range checks {
				want := base[ck.ID]
				if slices.Contains(hostTable[m], ck.ID) {
					want = c.Fail
				}
				got := hostOne(t, w, ck, m)
				if got.Status != want {
					t.Errorf("%v %s under %q: %s (%s), want %s", w, ck.ID, m, got.Status, got.Reason, want)
				}
				if got.Status == c.Fail {
					failedSomewhere[m] = true
					covered[ck.ID] = true
				}
			}
		}
	}
	for _, m := range fake.HostMutants {
		if !failedSomewhere[m] {
			t.Errorf("mutant %q fails nothing", m)
		}
	}
	for _, ck := range append(HostChecks(), TrustChecks()...) {
		if !covered[ck.ID] {
			t.Errorf("%s is failed by no mutant", ck.ID)
		}
	}
}

// Every check needs its positive control and the host's calls: a model
// whose link, scripts, shell or sandbox are broken, or a host without
// the calls, gives ERROR, never PASS.
func TestHostNoVacuousPass(t *testing.T) {
	hostOnly := map[string]bool{"T1": true, "T5": true, "T6": true}
	faults := []struct {
		name string
		set  func(*fake.Link)
		// sandbox is whether only the checks that use the sandbox are hit.
		sandbox bool
	}{
		{"link down", func(l *fake.Link) { l.SetLinkDown(true) }, true},
		{"scripts silent", func(l *fake.Link) { l.SetScriptsSilent(true) }, true},
		{"shell missing", func(l *fake.Link) { l.SetShellMissing(true) }, true},
		{"sandbox down", func(l *fake.Link) { l.SetDown(true) }, true},
		{"alias-out missing", func(l *fake.Link) { l.HideCanary(RoleAliasOut) }, true},
		{"h1-out missing", func(l *fake.Link) { l.HideCanary(RoleHostOut) }, true},
	}
	for _, w := range hostWorlds {
		for _, f := range faults {
			for _, ck := range ForRun(w.iso, w.trust) {
				l := newLink(t, w)
				f.set(l)
				got := hostRunIn(t, l, l.Env(w.iso, w.trust), []c.Check{ck})[ck.ID]
				if hostOnly[ck.ID] {
					continue
				}
				if ck.ID == "T2" && f.name != "sandbox down" {
					continue // T2 talks to the launcher, not the sandbox
				}
				if ck.ID == "T4" && f.name != "sandbox down" && f.name != "h1-out missing" {
					continue // T4 reads labels only
				}
				if got.Status != c.Error {
					t.Errorf("%v %s with %s: %s (%s), want ERROR", w, ck.ID, f.name, got.Status, got.Reason)
				}
			}
		}
		// A host without the calls: every check is an ERROR.
		for _, ck := range ForRun(w.iso, w.trust) {
			l := newLink(t, w)
			env := c.NewEnv(l, l.Fake, l, w.iso, w.trust)
			if got := hostRunIn(t, l, env, []c.Check{ck})[ck.ID]; got.Status != c.Error {
				t.Errorf("%v %s with no host calls: %s (%s), want ERROR", w, ck.ID, got.Status, got.Reason)
			}
		}
	}
}

// With the stubs missing, no host check sends anything to the sandbox,
// so nothing can reach the person at the host, and none passes.
func TestHostStubsMissingSendsNothing(t *testing.T) {
	for _, w := range hostWorlds {
		for _, ck := range ForRun(w.iso, w.trust) {
			if ck.Group != groupHost && ck.ID != "T3" {
				continue
			}
			for _, m := range append([]fake.Mutant{fake.Enforcing}, fake.HostMutants...) {
				l := newLink(t, w)
				l.Break(m)
				l.SetStubsMissing(true)
				got := hostRunIn(t, l, l.Env(w.iso, w.trust), []c.Check{ck})[ck.ID]
				if got.Status != c.Error {
					t.Errorf("%v %s under %q without stubs: %s, want ERROR", w, ck.ID, m, got.Status)
				}
				for _, ev := range l.Events() {
					if strings.HasPrefix(ev, "exec") {
						t.Errorf("%v %s under %q without stubs sent a command", w, ck.ID, m)
						break
					}
				}
				if l.Leaked() != 0 {
					t.Errorf("%v %s under %q: a notification reached no stub", w, ck.ID, m)
				}
			}
		}
	}
}

// H6's positive control: a stub that records nothing is an ERROR.
type deafStubs struct{ *fake.Link }

func (deafStubs) StubCalls() (int, error) { return 0, nil }

func TestH6NeedsTheStubToHear(t *testing.T) {
	w := hostWorld{"vm", c.TrustFallible}
	l := newLink(t, w)
	d := deafStubs{l}
	env := c.NewEnv(d, d, d, w.iso, w.trust)
	if got := hostRunIn(t, l, env, []c.Check{h6()})["H6"]; got.Status != c.Error || !strings.Contains(got.Reason, "stub") {
		t.Errorf("H6 with a deaf stub: %s %s, want an ERROR naming the stub", got.Status, got.Reason)
	}
}

// T2 puts config.toml back, and says so when it cannot.
type stuckTrust struct {
	*fake.Link
	calls int
}

func (s *stuckTrust) SetTrust(trust string) error {
	s.calls++
	if s.calls > 1 {
		return errors.New("read-only")
	}
	return s.Link.SetTrust(trust)
}

func TestT2RestoresTheConfig(t *testing.T) {
	w := hostWorld{"container", c.TrustFallible}
	l := newLink(t, w)
	if got := hostRunIn(t, l, l.Env(w.iso, w.trust), []c.Check{t2()})["T2"]; got.Status != c.Pass {
		t.Fatalf("T2: %s %s", got.Status, got.Reason)
	}
	if l.Trust() != c.TrustFallible {
		t.Errorf("T2 left trust %q", l.Trust())
	}
	l = newLink(t, w)
	s := &stuckTrust{Link: l}
	env := c.NewEnv(s, s, s, w.iso, w.trust)
	if got := hostRunIn(t, l, env, []c.Check{t2()})["T2"]; got.Status != c.Error || !strings.Contains(got.Reason, "config.toml") {
		t.Errorf("T2 that cannot restore: %s %s, want an ERROR naming config.toml", got.Status, got.Reason)
	}
}

// recording is a Sandbox that keeps every command it is sent.
type recording struct {
	*fake.Link
	mu   sync.Mutex
	sent [][]string
}

func (r *recording) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	r.mu.Lock()
	r.sent = append(r.sent, slices.Clone(argv))
	r.mu.Unlock()
	return r.Link.Exec(ctx, user, argv, stdin)
}

// agentForm is every command a host or trust check may send, with each
// word single-quoted.
var agentForm = regexp.MustCompile(`^(caboose-agent (run|notify)( '[A-Za-z0-9 ./=_-]+')+` +
	`|timeout [0-9]+ caboose-agent run '[a-z0-9-]+'` +
	`|caboose-agent host (-C /work/conf-[0-9a-f]{8}-ro/conf-[0-9a-f]{8}-[a-z0-9-]+ )?--( '[a-z0-9-]+')+` +
	`|caboose-agent aliases --json 1>&2` +
	`|echo '[^'$` + "`" + `\\]+' >> /home/agent/[A-Za-z0-9./-]+( && echo '[^'$` + "`" + `\\]+' >> /home/agent/[A-Za-z0-9./-]+)*)$`)

// The commands are literal: one of the forms above, no expansion, glob or
// tilde outside the quotes, and valid sh. The syntax is checked with the
// command name replaced, so no shell in this test is ever handed
// caboose-agent's name.
func TestHostCommandsAreLiteral(t *testing.T) {
	sent := map[string]bool{}
	for _, w := range hostWorlds {
		for _, m := range append([]fake.Mutant{fake.Enforcing}, fake.HostMutants...) {
			l := newLink(t, w)
			l.Break(m)
			r := &recording{Link: l}
			env := c.NewEnv(r, r, r, w.iso, w.trust)
			hostRunIn(t, l, env, ForRun(w.iso, w.trust))
			for _, argv := range r.sent {
				body, ok := c.Unwrap(argv)
				if !ok {
					t.Fatalf("not the wrapper: %q", argv)
				}
				sent[body] = true
			}
		}
	}
	if len(sent) < 15 {
		t.Fatalf("only %d distinct commands sent", len(sent))
	}
	unquoted := regexp.MustCompile(`'[^']*'`)
	for body := range sent {
		if !agentForm.MatchString(body) {
			t.Errorf("not a form the suite sends: %q", body)
		}
		if strings.ContainsAny(body, "$`~") {
			t.Errorf("expansion, backtick or tilde: %q", body)
		}
		if strings.ContainsAny(unquoted.ReplaceAllString(body, "''"), "*?[]") {
			t.Errorf("a glob outside quotes: %q", body)
		}
		standIn := strings.ReplaceAll(body, "caboose-agent", "conf-agent-stand-in")
		if strings.Contains(standIn, "caboose-agent") {
			t.Fatalf("the name survived: %q", standIn)
		}
		script := "LC_ALL=C; { " + standIn + "; } >/dev/null"
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("sh -n rejected %q: %v %s", standIn, err, out)
		}
	}
}

// With every mutant on at once, the model changes nothing but the
// checks' canaries.
func TestHostChecksTouchOnlyCanaries(t *testing.T) {
	for _, w := range hostWorlds {
		l := newLink(t, w)
		for _, m := range fake.HostMutants {
			l.Break(m)
		}
		hostRunIn(t, l, l.Env(w.iso, w.trust), ForRun(w.iso, w.trust))
		canaries := l.CanaryPaths()
		for _, mu := range l.Mutations() {
			ok := false
			for _, p := range canaries {
				if mu.Rel == p || strings.HasPrefix(mu.Rel, p+"/") {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%v: %s %s is no canary's", w, mu.Op, mu.Rel)
			}
		}
	}
}

func TestHostLayout(t *testing.T) {
	if _, err := HostLayout("", "roots/x"); err == nil {
		t.Error("no canary directory accepted")
	}
	got, err := HostLayout("canary", "roots/conf-0a1b2c3d-ro")
	if err != nil || len(got) != 2 || got[0].Role != RoleAliasOut || got[1].Role != RoleHostOut {
		t.Errorf("HostLayout: %v %v", got, err)
	}
}

// The check files never run anything themselves: no os/exec, os, net or
// syscall, and no string that names Claude Code, so no check can start
// it; nor any of the names the suite never opens.
func TestHostSourceIsInert(t *testing.T) {
	banned := map[string]bool{"os": true, "os/exec": true, "net": true, "syscall": true, "golang.org/x/sys/unix": true, "plugin": true, "unsafe": true}
	for _, file := range []string{"hostcmd.go", "trust.go"} {
		fs := token.NewFileSet()
		f, err := parser.ParseFile(fs, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if banned[p] {
				t.Errorf("%s imports %s", file, p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, _ := strconv.Unquote(lit.Value)
			low := strings.ToLower(s)
			if regexp.MustCompile(`^(timeout [0-9]+ )?caboose-agent [a-z]`).MatchString(s) {
				t.Errorf("%s:%d: a caboose-agent command outside the builders: %q", file, fs.Position(lit.Pos()).Line, s)
			}
			for _, bad := range []string{"claude", ".ssh", ".credentials", "ssh_auth_sock", "/users/", "$home", "keychain"} {
				if strings.Contains(low, bad) && !(bad == "claude" && strings.Contains(s, "Claude Code process")) {
					t.Errorf("%s:%d: a string naming %q: %q", file, fs.Position(lit.Pos()).Line, bad, s)
				}
			}
			return true
		})
	}
}

// The scan above catches what it looks for.
func TestHostSourceScanCatches(t *testing.T) {
	src := `package p
import "os/exec"
var s = "claude --print"
`
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := strconv.Unquote(f.Imports[0].Path.Value); p != "os/exec" {
		t.Fatal("import not seen")
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && strings.Contains(lit.Value, "claude") {
			found = true
		}
		return true
	})
	if !found {
		t.Error("string not seen")
	}
}

func TestSuiteAliases(t *testing.T) {
	as, err := c.SuiteAliases("/tmp/caboose-conf.1/canary/bin", "/tmp/caboose-conf.1/canary/conf-0a1b2c3d-alias-out", "/tmp/caboose-conf.1/roots/conf-0a1b2c3d-dev/part")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, a := range as {
		names[a.Name] = true
		if !strings.HasPrefix(a.Command[0], "/tmp/caboose-conf.1/canary/bin/") {
			t.Errorf("%s runs %s", a.Name, a.Command[0])
		}
		if out, err := exec.Command("sh", "-n", "-c", a.Script).CombinedOutput(); err != nil {
			t.Errorf("%s's script: %v %s", a.Name, err, out)
		}
		for _, l := range strings.Split(a.Script, "\n") {
			if strings.Contains(l, ">") && !strings.Contains(l, "'/tmp/caboose-conf.1/canary/conf-0a1b2c3d-alias-out/") {
				t.Errorf("%s writes outside alias-out: %q", a.Name, l)
			}
		}
	}
	for _, n := range []string{c.AliasMarker, c.AliasAsk, c.AliasWritable, c.AliasSleep} {
		if !names[n] {
			t.Errorf("no alias %s", n)
		}
	}
	for _, bad := range []string{"relative/dir", "/tmp/../etc", "/tmp/x'y", "/tmp/x y", "/tmp/$x"} {
		if _, err := c.SuiteAliases(bad, "/tmp/o", "/tmp/p"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := c.AliasScript("conf-nosuch", "/tmp/o"); err == nil {
		t.Error("an alias the suite does not define has a script")
	}
}

func TestAgentBuildersRefuse(t *testing.T) {
	bad := []func() error{
		func() error { _, err := c.AgentRun("x'y"); return err },
		func() error { _, err := c.AgentRun("a", "$(id)"); return err },
		func() error { _, err := c.AgentRun(strings.Repeat("a", 65)); return err },
		func() error { _, err := c.AgentRunFor(0, "conf-sleep"); return err },
		func() error { _, err := c.AgentRunFor(11, "conf-sleep"); return err },
		func() error { _, err := c.AgentHost(nil); return err },
		func() error { _, err := c.AgentHost(nil, "/bin/sh"); return err },
		func() error { _, err := c.AgentHost(&c.SPath{}, "true"); return err },
		func() error { _, err := c.AgentNotify("`id`"); return err },
		func() error { _, err := c.AppendLines(c.SandboxConfigFile, "a'b"); return err },
		func() error { _, err := c.AppendLines(c.SandboxConfigFile, "$HOME"); return err },
		func() error { _, err := c.AppendLines(c.HomeFile{}, "x"); return err },
		func() error { _, err := c.ProposalFile("xyz", "alias"); return err },
		func() error { _, err := c.SyncFile(hostN8, "../x"); return err },
	}
	for i, f := range bad {
		if f() == nil {
			t.Errorf("builder %d accepted a bad word", i)
		}
	}
	cmd, err := c.AgentRun(c.AliasMarker, "mode=one")
	if err != nil || cmd.Body() != "caboose-agent run 'conf-marker' 'mode=one'" {
		t.Errorf("AgentRun: %q %v", cmd.Body(), err)
	}
	if cmd.Destructive() {
		t.Error("an alias run is destructive")
	}
}
