package checks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bfreis/caboose/internal/agentproto"
	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/fake"
	"github.com/bfreis/caboose/internal/conformance/probe"
)

// daemonWorlds are the isolations and trusts the daemon checks run under:
// under hostile without vm no sandbox is made, and they do not run.
var daemonWorlds = []hostWorld{
	{"container", c.TrustFallible}, {"gvisor", c.TrustFallible}, {"vm", c.TrustFallible}, {"vm", c.TrustHostile},
}

// newDaemon is a Daemon of world w with the daemon checks' canaries made
// from DaemonLayout, as the suite makes them.
func newDaemon(t testing.TB, w hostWorld) *fake.Daemon {
	t.Helper()
	d, dirs, err := fake.NewDaemon(hostN8, w.iso, w.trust)
	if err != nil {
		t.Fatal(err)
	}
	fake.NoAgentRuns(t, d.Fake)
	specs, err := DaemonLayout(DaemonDirs(dirs))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if _, err := d.MakeCanary(s.Parent, s.Role, s.Kind); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func daemonRunIn(t testing.TB, d *fake.Daemon, env c.Env, checks []c.Check) (map[string]c.Result, *c.Collector) {
	t.Helper()
	r := &c.Runner{Env: &fake.Envelope{F: d.Fake}}
	res, col, err := c.RunChecks(context.Background(), r, checks, env)
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
	return out, col
}

// daemonBase is each check's status with every control holding.
var daemonBase = map[string]c.Status{
	"D0": c.Pass, "D1": c.Pass, "D2": c.Pass, "D3": c.Gap, "D4": c.Pass, "D5": c.Pass,
	"D6": c.Pass, "D7": c.Pass, "D8": c.Pass, "D9": c.Pass, "X1": c.Gap, "X2": c.Gap,
}

// baseIn is a check's status with every control holding in world w: D7
// is skipped under hostile, which asks before every URL, and D3 passes
// under vm, whose profiles take no run_args.
func baseIn(w hostWorld, id string) c.Status {
	if id == "D7" && w.trust == c.TrustHostile {
		return c.Skip
	}
	if id == "D3" && w.iso == "vm" {
		return c.Pass
	}
	return daemonBase[id]
}

func daemonByID(id string) c.Check {
	for _, ck := range DaemonChecks() {
		if ck.ID == id {
			return ck
		}
	}
	panic("no check " + id)
}

func TestDaemonChecksEnforcing(t *testing.T) {
	for _, w := range daemonWorlds {
		checks := DaemonForRun(w.iso, w.trust)
		if len(checks) != len(daemonBase) {
			t.Fatalf("%v: %d checks, want %d", w, len(checks), len(daemonBase))
		}
		d := newDaemon(t, w)
		got, _ := daemonRunIn(t, d, d.Env(w.iso, w.trust), checks)
		for _, ck := range checks {
			if r := got[ck.ID]; r.Status != baseIn(w, ck.ID) {
				t.Errorf("%v %s: %s %s, want %s", w, ck.ID, r.Status, r.Reason, baseIn(w, ck.ID))
			}
		}
		if got["X1"].Reason != gapX1Open || got["X2"].Reason != gapX2 {
			t.Errorf("%v: gap lines %q, %q", w, got["X1"].Reason, got["X2"].Reason)
		}
		if w.iso != "vm" && !strings.Contains(got["D3"].Reason, "--network host is allowed under trust = fallible, warned by doctor and caboose ui") {
			t.Errorf("%v: D3's gap is %q", w, got["D3"].Reason)
		}
		if n := d.Leaked(); n != 0 {
			t.Errorf("%v: %d notifications or URLs reached no stub", w, n)
		}
		if err := fake.CheckOrder(d.Events()); err != nil {
			t.Errorf("%v: %v", w, err)
		}
	}
}

// TestD3UnderVM holds D3 to its PASS under vm, fallible and hostile
// alike: the run_args case does not apply, every other reach is still
// refused, and a mutant letting one through still fails it.
func TestD3UnderVM(t *testing.T) {
	for _, w := range daemonWorlds {
		if w.iso != "vm" {
			continue
		}
		d := newDaemon(t, w)
		got, col := daemonRunIn(t, d, d.Env(w.iso, w.trust), []c.Check{daemonByID("D3")})
		if r := got["D3"]; r.Status != c.Pass {
			t.Errorf("%v: D3 is %s %q, want PASS", w, r.Status, r.Reason)
		}
		lines := col.Lines("D3")
		if !slices.Contains(lines, "run_args --network host: does not apply, a vm profile takes no run_args") {
			t.Errorf("%v: D3's lines %q do not say the run_args case does not apply", w, lines)
		}
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "run_args publishes onto ") }) {
			t.Errorf("%v: D3's lines %q skip the publish reach", w, lines)
		}
		if r := daemonOne(t, w, daemonByID("D3"), fake.UIPublishAllowed); r.Status != c.Fail {
			t.Errorf("%v: D3 with --publish allowed: %s %s", w, r.Status, r.Reason)
		}
	}
}

func TestDaemonForRun(t *testing.T) {
	for _, iso := range []string{"container", "gvisor"} {
		if got := DaemonForRun(iso, c.TrustHostile); len(got) != 0 {
			t.Errorf("hostile under %s runs %d daemon checks", iso, len(got))
		}
	}
	seen := map[string]bool{}
	for _, ck := range DaemonChecks() {
		if seen[ck.ID] {
			t.Errorf("%s twice", ck.ID)
		}
		seen[ck.ID] = true
		if ck.Destructive() || ck.Group != groupDaemon || ck.Control == "" {
			t.Errorf("%s: class %s, group %q, control %q", ck.ID, ck.Class, ck.Group, ck.Control)
		}
		if _, ok := daemonBase[ck.ID]; !ok {
			t.Errorf("%s has no base status", ck.ID)
		}
	}
	if len(seen) != len(daemonBase) {
		t.Errorf("%d checks, want D0-D9, X1 and X2", len(seen))
	}
}

// daemonTable maps each mutant to the checks it must fail. TrustFromSandbox
// reads a lowered trust only, so it is held to D6 under hostile alone.
var daemonTable = map[fake.Mutant][]string{
	fake.DaemonDirMounted:      {"D0", "D1", "D2"},
	fake.SocketReachable:       {"D2"},
	fake.WebPortOpen:           {"D4"},
	fake.AnswerAccepted:        {"D5"},
	fake.SandboxWritesConfig:   {"D5", "D6"},
	fake.EnvDirMounted:         {"D0", "D1"},
	fake.RootUnderHome:         {"D0"},
	fake.UIForwardDoctorOnly:   {"D3"},
	fake.RateLimitSilent:       {"D8"},
	fake.RequestAllowedNoGrant: {"D5"},
	fake.TermSucceeds:          {"D9"},
	fake.ForeignSocketMounted:  {"D2"},
	fake.BoundSocketIsDaemon:   {"D2"},
	fake.SilentRelayToDaemon:   {"D2"},
	fake.AgentRelayToDaemon:    {"D2"},
	fake.HelloRelayDropsReply:  {"D2"},
	fake.OutsideMounted:        {"D0", "D1"},
	fake.CredsMounted:          {"D0", "D1"},
	fake.CanaryCopied:          {"D1"},
	fake.RootMountedTwice:      {"D0"},
	fake.PartDirInHome:         {"D0"},
	fake.UIForwardAllowed:      {"D3"},
	fake.UIURLAllowedInConfig:  {"D3"},
	fake.UIEgressAllowed:       {"D3"},
	fake.UIPublishAllowed:      {"D3"},
	fake.RequestTextAnswers:    {"D5"},
	fake.RequestsApproveFlag:   {"D5"},
	fake.GrantedWhileRefused:   {"D5"},
	fake.ProposalAnswers:       {"D5"},
	fake.SandboxRootsApplied:   {"D6"},
	fake.UIURLOpens:            {"D7"},
	fake.URLSpellingsMissed:    {"D7"},
	fake.URLRefusedButOpened:   {"D7"},
	fake.RateLimitOff:          {"D8"},
	fake.FloodKillsLink:        {"D8"},
	fake.FloodLeaksMemory:      {"D8"},
	fake.FloodRestartsDaemon:   {"D8"},
	fake.DaemonPidVisible:      {"D9"},
	fake.DaemonKillable:        {"D9"},
}

// daemonClosers close a gap: no check fails, and the one named changes.
var daemonClosers = map[fake.Mutant]map[string]c.Status{
	fake.NetworkHostRefused: {"D3": c.Pass},
	fake.HomeReadOnly:       {"X1": c.Gap},
}

func daemonOne(t testing.TB, w hostWorld, ck c.Check, m fake.Mutant) c.Result {
	t.Helper()
	d := newDaemon(t, w)
	d.Break(m)
	got, _ := daemonRunIn(t, d, d.Env(w.iso, w.trust), []c.Check{ck})
	return got[ck.ID]
}

// TestDaemonMutants holds every check to its mutants, each check alone in
// a fresh world: the assigned checks FAIL, every other is as enforcing.
func TestDaemonMutants(t *testing.T) {
	for _, m := range fake.DaemonMutants {
		_, fails := daemonTable[m]
		_, closes := daemonClosers[m]
		if !fails && !closes && m != fake.TrustFromSandbox {
			t.Errorf("mutant %q has no row", m)
		}
	}
	covered := map[string]bool{}
	for _, w := range daemonWorlds {
		table := map[fake.Mutant][]string{}
		for m, ids := range daemonTable {
			table[m] = ids
		}
		if w.trust == c.TrustHostile {
			table[fake.TrustFromSandbox] = []string{"D6"}
		}
		for m, ids := range table {
			for _, ck := range DaemonChecks() {
				want := baseIn(w, ck.ID)
				if slices.Contains(ids, ck.ID) && want != c.Skip {
					want = c.Fail
					covered[ck.ID] = true
				}
				if r := daemonOne(t, w, ck, m); r.Status != want {
					t.Errorf("%v %s under %q: %s (%s), want %s", w, ck.ID, m, r.Status, r.Reason, want)
				}
			}
		}
		for m, row := range daemonClosers {
			for _, ck := range DaemonChecks() {
				want, ok := row[ck.ID]
				if !ok || baseIn(w, ck.ID) == c.Skip {
					want = baseIn(w, ck.ID)
				}
				if r := daemonOne(t, w, ck, m); r.Status != want {
					t.Errorf("%v %s under %q: %s (%s), want %s", w, ck.ID, m, r.Status, r.Reason, want)
				}
			}
		}
	}
	for id, st := range daemonBase {
		if st != c.Gap && !covered[id] {
			t.Errorf("check %s has no mutant that fails it", id)
		}
	}
	w := daemonWorlds[0]
	if r := daemonOne(t, w, daemonByID("D3"), fake.NetworkHostRefused); r.Status != c.Pass {
		t.Errorf("D3 with --network host refused: %s %s", r.Status, r.Reason)
	}
	if r := daemonOne(t, w, daemonByID("X1"), fake.HomeReadOnly); r.Reason != gapX1Closed {
		t.Errorf("X1 with the home read-only: %q", r.Reason)
	}
}

// daemonFaults are what each fault must make of the checks it touches:
// an answer the check was not written for is ERROR (or SKIP where the
// check says it cannot run), never PASS.
var daemonFaults = map[fake.Fault]map[string]c.Status{
	fake.ScanCutShort:       {"D0": c.Error, "D1": c.Error},
	fake.DScanBlind:         {"D0": c.Error, "D1": c.Error},
	fake.ContentBlind:       {"D0": c.Error, "D1": c.Error},
	fake.SocketsUnlisted:    {"D2": c.Error},
	fake.ProbeMissing:       {"D2": c.Error, "D5": c.Error},
	fake.ProbeSwapped:       {"D2": c.Error, "D5": c.Error},
	fake.ProbeUnproven:      {"D2": c.Error},
	fake.DaemonDown:         {"D2": c.Error, "D4": c.Error, "D8": c.Error, "D9": c.Error, "X2": c.Error},
	fake.UINotOwned:         {"D4": c.Skip},
	fake.UIDown:             {"D4": c.Error},
	fake.DCurlMissing:       {"D4": c.Error},
	fake.UIRefusesAll:       {"D3": c.Error},
	fake.UIRefusalUnnamed:   {"D3": c.Error},
	fake.RequestNotFiled:    {"D5": c.Error},
	fake.OpenURLsAsk:        {"D7": c.Skip},
	fake.StubOpenerDeaf:     {"D7": c.Error},
	fake.MountsDrift:        {"D6": c.Error},
	fake.NoRoot:             {"D9": c.Error},
	fake.RSSUnreadable:      {"D8": c.Error},
	fake.SocketFindFails:    {"D2": c.Error},
	fake.ProbeControlFails:  {"D5": c.Error},
	fake.LinkOpsUnproven:    {"D5": c.Error},
	fake.HomeCanaryUnseen:   {"X1": c.Error},
	fake.HomeAppendUnseen:   {"X1": c.Error},
	fake.SocketFindCutShort: {"D2": c.Error},
	fake.ProbeControlOnly:   {"D5": c.Error},
	fake.SameSocketBlind:    {"D2": c.Error},
	fake.SocketVanishes:     {"D2": c.Error},
	fake.WitnessBlind:       {"D2": c.Error},
	fake.HoldersUnlisted:    {"D2": c.Error},
	fake.LogRotates:         {"D2": c.Error},
	fake.HoldersCutShort:    {"D2": c.Error},
	fake.AgentExeImpostor:   {"D2": c.Error},
	// The agent's file gone from its path is still the agent: D2 as it
	// was.
	fake.AgentExeDeleted: {},
}

func TestDaemonFaults(t *testing.T) {
	for _, f := range fake.DaemonFaults {
		if _, ok := daemonFaults[f]; !ok {
			t.Errorf("fault %q has no row", f)
		}
	}
	w := hostWorld{"vm", c.TrustFallible}
	for f, row := range daemonFaults {
		for _, ck := range DaemonChecks() {
			want, ok := row[ck.ID]
			if !ok {
				want = baseIn(w, ck.ID)
			}
			d := newDaemon(t, w)
			d.SetFault(f)
			got, _ := daemonRunIn(t, d, d.Env(w.iso, w.trust), []c.Check{ck})
			if r := got[ck.ID]; r.Status != want {
				t.Errorf("%s under %q: %s (%s), want %s", ck.ID, f, r.Status, r.Reason, want)
			}
		}
	}
}

// No check passes with the sandbox down, its shell gone, the stubs away,
// the link down or a canary missing: the checks that need what is gone
// are ERROR, the others as they were.
func TestDaemonNoVacuousPass(t *testing.T) {
	w := hostWorld{"container", c.TrustFallible}
	all := []string{"D0", "D1", "D2", "D3", "D4", "D5", "D6", "D7", "D8", "D9", "X1", "X2"}
	sandbox := []string{"D0", "D1", "D2", "D4", "D5", "D6", "D7", "D8", "D9", "X1"}
	for name, x := range map[string]struct {
		setup  func(d *fake.Daemon)
		errors []string
	}{
		"sandbox down":   {func(d *fake.Daemon) { d.SetDown(true) }, all},
		"stubs missing":  {func(d *fake.Daemon) { d.SetStubsMissing(true) }, all},
		"home canary":    {func(d *fake.Daemon) { d.HideCanary(RoleDHome) }, all},
		"shell missing":  {func(d *fake.Daemon) { d.SetShellMissing(true) }, sandbox},
		"link down":      {func(d *fake.Daemon) { d.SetLinkDown(true) }, []string{"D5", "D7", "D8"}},
		"root canary":    {func(d *fake.Daemon) { d.HideCanary(RoleDRoot) }, []string{"D0", "D1"}},
		"daemon canary":  {func(d *fake.Daemon) { d.HideCanary(RoleDDaemon) }, []string{"D1"}},
		"x1 canary":      {func(d *fake.Daemon) { d.HideCanary(RoleX1) }, []string{"X1"}},
		"alias-out gone": {func(d *fake.Daemon) { d.HideCanary(RoleAliasOut) }, []string{"D5"}},
	} {
		for _, ck := range DaemonChecks() {
			d := newDaemon(t, w)
			x.setup(d)
			got, _ := daemonRunIn(t, d, d.Env(w.iso, w.trust), []c.Check{ck})
			want := baseIn(w, ck.ID)
			if slices.Contains(x.errors, ck.ID) {
				want = c.Error
			}
			if r := got[ck.ID]; r.Status != want {
				t.Errorf("%s with %s: %s %s, want %s", ck.ID, name, r.Status, r.Reason, want)
			}
		}
	}
}

// Without the stubs nothing is sent at all.
func TestDaemonStubsMissingSendsNothing(t *testing.T) {
	w := hostWorld{"vm", c.TrustFallible}
	d := newDaemon(t, w)
	d.SetStubsMissing(true)
	r := &recordingD{daemonModel: d}
	daemonRunIn(t, d, c.NewEnv(r, r, r, w.iso, w.trust), DaemonChecks())
	if len(r.sent) != 0 {
		t.Errorf("%d commands sent without the stubs: %q", len(r.sent), r.sent[0])
	}
	if d.Leaked() != 0 {
		t.Errorf("%d reached no stub", d.Leaked())
	}
}

// daemonModel names the fake's type for embedding: a field named Daemon
// would hide its Daemon method.
type daemonModel = fake.Daemon

type recordingD struct {
	*daemonModel
	mu   sync.Mutex
	sent [][]string
}

func (r *recordingD) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	r.mu.Lock()
	r.sent = append(r.sent, slices.Clone(argv))
	r.mu.Unlock()
	return r.daemonModel.Exec(ctx, user, argv, stdin)
}

func (r *recordingD) bodies(t *testing.T) []string {
	var out []string
	for _, argv := range r.sent {
		b, ok := c.Unwrap(argv)
		if !ok {
			t.Fatalf("not the wrapper: %q", argv)
		}
		out = append(out, b)
	}
	return out
}

// D2 hands the probe only the sockets the sandbox listed, never the
// link's own, and tries every one of them.
func TestD2ProbesOnlyFoundSockets(t *testing.T) {
	for _, m := range []fake.Mutant{fake.Enforcing, fake.SocketReachable} {
		w := hostWorld{"container", c.TrustFallible}
		d := newDaemon(t, w)
		d.Break(m)
		r := &recordingD{daemonModel: d}
		daemonRunIn(t, d, c.NewEnv(r, r, r, w.iso, w.trust), []c.Check{daemonByID("D2")})
		daemonFlag := regexp.MustCompile(`--daemon '([^']+)'`)
		var tried []string
		for _, b := range r.bodies(t) {
			if mm := daemonFlag.FindStringSubmatch(b); mm != nil {
				tried = append(tried, mm[1])
			}
		}
		want := []string{"/tmp/conf-stale-fake.sock"}
		if m == fake.SocketReachable {
			want = []string{"/run/caboose-daemon.sock"}
		}
		if !slices.Equal(tried, want) {
			t.Errorf("%q: tried %q, want %q", m, tried, want)
		}
	}
}

// The mount audit alone is enough for D2 to FAIL: a mount holding the
// daemon's directory, whatever the probe says.
type mountsDaemonDir struct{ *daemonModel }

func (m mountsDaemonDir) Mounts() []c.Mount {
	return append(m.daemonModel.Mounts(), c.Mount{Source: "<B>/home", Target: "/mnt/home", ReadOnly: true})
}

func TestD2ReadsTheMounts(t *testing.T) {
	w := hostWorld{"container", c.TrustFallible}
	d := newDaemon(t, w)
	m := mountsDaemonDir{d}
	got, _ := daemonRunIn(t, d, c.NewEnv(m, d, d, w.iso, w.trust), []c.Check{daemonByID("D2")})
	if r := got["D2"]; r.Status != c.Fail || !strings.Contains(r.Reason, "daemon's directory") {
		t.Errorf("D2 with B/home mounted: %s %s", r.Status, r.Reason)
	}
}

// D4 sends nothing when the port is not the throwaway daemon's; D7 sends
// nothing past its control when the stub did not hear it.
func TestD4AndD7SendNothingUnsafe(t *testing.T) {
	w := hostWorld{"vm", c.TrustFallible}
	d := newDaemon(t, w)
	d.SetFault(fake.UINotOwned)
	r := &recordingD{daemonModel: d}
	daemonRunIn(t, d, c.NewEnv(r, r, r, w.iso, w.trust), []c.Check{daemonByID("D4")})
	if len(r.sent) != 0 {
		t.Errorf("D4 sent %d commands to a port not the throwaway daemon's", len(r.sent))
	}
	d = newDaemon(t, w)
	d.SetFault(fake.StubOpenerDeaf)
	r = &recordingD{daemonModel: d}
	daemonRunIn(t, d, c.NewEnv(r, r, r, w.iso, w.trust), []c.Check{daemonByID("D7")})
	if b := r.bodies(t); len(b) != 1 || !strings.Contains(b[0], harmlessURL) {
		t.Errorf("D7 sent %q with a deaf stub", b)
	}
}

// Under hostile, D7 is SKIP and sends nothing: its positive control would
// file a real ask on the throwaway daemon, and the check must not ERROR
// (which would stop every later destructive check).
func TestD7SkipsUnderHostile(t *testing.T) {
	w := hostWorld{"vm", c.TrustHostile}
	d := newDaemon(t, w)
	r := &recordingD{daemonModel: d}
	got, _ := daemonRunIn(t, d, c.NewEnv(r, r, r, w.iso, w.trust), []c.Check{daemonByID("D7")})
	if res := got["D7"]; res.Status != c.Skip || !strings.Contains(res.Reason, "hostile") {
		t.Errorf("D7 under hostile: %s %s", res.Status, res.Reason)
	}
	if b := r.bodies(t); len(b) != 0 {
		t.Errorf("D7 under hostile sent %q", b)
	}
}

// A lying sandbox: a URL reported refused that reached the opener is a
// FAIL, from the stub's record.
func TestD7JudgesFromTheStub(t *testing.T) {
	r := daemonOne(t, daemonWorlds[0], daemonByID("D7"), fake.URLRefusedButOpened)
	if r.Status != c.Fail || !strings.Contains(r.Reason, "reached the opener") {
		t.Errorf("D7 with a lying refusal: %s %s", r.Status, r.Reason)
	}
}

// daemonForm is every command a daemon check may send.
var daemonForm = regexp.MustCompile(`^(` +
	`\{ timeout 60 find / '\(' -path /proc -o -path /sys -o -path /dev '\)' -prune -o -print 2>/dev/null && echo conf-[0-9a-f]{8}-scan-complete; \} \| head -n 2000000 \| grep -x -E '\.\*conf-[0-9a-f]{8}-\[\^/\]\*' \| head -n 4001 1>&2` +
	`|\{ timeout 60 find /work /home/agent -type f -size -10240k -exec grep -l -F -e [0-9a-z-]+ '\{\}' ';' 2>/dev/null && echo conf-[0-9a-f]{8}-scan-complete; \} \| head -n 4001 1>&2` +
	`|\{ timeout 60 find / '\(' -path /proc -o -path /sys -o -path /dev '\)' -prune -o -type s -print 2>/dev/null && echo conf-[0-9a-f]{8}-scan-complete; \} \| head -n 64 1>&2` +
	`|head -n 2000 /proc/net/unix 1>&2` +
	`|\{ timeout 60 find /proc -mindepth 3 -maxdepth 3 -path '/proc/\[0-9\]\*/fd/\*' -type l -exec readlink '\{\}' ';' -print 2>/dev/null; echo conf-[0-9a-f]{8}-scan-complete \$\?; \} \| grep -F -A1 -e 'socket:\[' -e conf-[0-9a-f]{8}-scan-complete \| head -n 8000 1>&2` +
	`|readlink /proc/[0-9]+/exe 1>&2` +
	`|if test -S '[A-Za-z0-9./_-]+'; then( test '[A-Za-z0-9./_-]+' -ef '[A-Za-z0-9./_-]+' && echo '[A-Za-z0-9./_-]+';)+ else echo 'conformance-socket-gone'; fi 1>&2; true` +
	`|/work/conf-[0-9a-f]{8}-dev/part/\.conformance-probe --nonce [0-9a-z-]+ --link '[A-Za-z0-9./_-]+' (--daemon '[A-Za-z0-9./_-]+' --only daemon-(no-)?hello|--only [a-z,]+) --timeout 10 1>&2` +
	`|caboose-agent (open '[A-Za-z0-9:/.\[\]#=_-]+'|notify '[A-Za-z0-9 -]+'|request alias --alias '[a-z-]+' --context '[A-Za-z0-9 -]+' --json 1>&2|requests '--approve' '[0-9]+')` +
	`|\{ for i in [0-9 ]+; do caboose-agent [^;&|]+( 1>&2)? & done; wait; \} 2>&1 \| grep -c -F 'too many requests' 1>&2` +
	`|kill -(0|TERM) [0-9]+|cat /proc/[0-9]+/cmdline 1>&2` +
	`|curl -g -sS -o /dev/null -w 'http=%\{http_code\}' --max-time 5 -X POST -H 'Origin: http://[a-z0-9.:-]+' -H 'Sec-Fetch-Site: same-origin' -H 'Content-Type: application/json'( -H '[A-Za-z-]+: [a-z0-9.-]+')? --data '\{\}' 'http://[a-z0-9.:\[\]-]+:717[12]/api/requests' 1>&2` +
	`|cat /home/agent/\.config/caboose/conf-[0-9a-f]{8}-[a-z0-9-]+ 1>&2|echo x >> /home/agent/\.config/caboose/conf-[0-9a-f]{8}-[a-z0-9-]+` +
	`|echo '[^'$` + "`" + `\\]+' >> /home/agent/[A-Za-z0-9./-]+( && echo '[^'$` + "`" + `\\]+' >> /home/agent/[A-Za-z0-9./-]+)*` +
	`)$`)

// Every command is one of the forms above: no expansion, glob or tilde
// outside quotes, and valid sh (checked with caboose-agent's name and the
// probe's path replaced, so no shell here is handed either).
func TestDaemonCommandsAreLiteral(t *testing.T) {
	sent := map[string]bool{}
	for _, w := range daemonWorlds {
		for _, m := range append([]fake.Mutant{fake.Enforcing}, fake.DaemonMutants...) {
			d := newDaemon(t, w)
			d.Break(m)
			r := &recordingD{daemonModel: d}
			daemonRunIn(t, d, c.NewEnv(r, r, r, w.iso, w.trust), DaemonChecks())
			for _, b := range r.bodies(t) {
				sent[b] = true
			}
		}
	}
	if len(sent) < 25 {
		t.Fatalf("only %d distinct commands sent", len(sent))
	}
	unquoted := regexp.MustCompile(`'[^']*'`)
	for body := range sent {
		if !daemonForm.MatchString(body) {
			t.Errorf("not a form the suite sends: %q", body)
		}
		// find's exit status is the one expansion, after the holders'
		// completion mark; ? there is no glob.
		lit := strings.Replace(body, "-scan-complete $?;", "-scan-complete;", 1)
		if strings.ContainsAny(lit, "$`~\\") {
			t.Errorf("expansion, backtick, tilde or backslash: %q", body)
		}
		if strings.ContainsAny(unquoted.ReplaceAllString(lit, "''"), "*?[]") {
			t.Errorf("a glob outside quotes: %q", body)
		}
		standIn := strings.ReplaceAll(body, "caboose-agent", "conf-agent-stand-in")
		standIn = regexp.MustCompile(`/work/conf-[0-9a-f]{8}-dev/part/\.conformance-probe`).ReplaceAllString(standIn, "conf-probe-stand-in")
		script := "LC_ALL=C; { " + standIn + "; } >/dev/null"
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("not valid sh: %q: %v %s", body, err, out)
		}
	}
}

// Whatever is broken, the checks change no host file but the canaries
// they are handed (the runner's post-check), and the probe and the
// alias-out canary stay as they were.
func TestDaemonChecksTouchOnlyCanaries(t *testing.T) {
	w := hostWorld{"vm", c.TrustFallible}
	for _, m := range append([]fake.Mutant{fake.Enforcing}, fake.DaemonMutants...) {
		d := newDaemon(t, w)
		d.Break(m)
		daemonRunIn(t, d, d.Env(w.iso, w.trust), DaemonChecks())
		allowed := map[string]bool{}
		for _, p := range d.CanaryPaths() {
			allowed[p] = true
		}
		for _, mu := range d.Mutations() {
			if !allowed[mu.Rel] && !strings.HasSuffix(mu.Rel, "/ran") {
				t.Errorf("%q: %s %s is no canary", m, mu.Op, mu.Rel)
			}
		}
	}
}

func TestDaemonLayout(t *testing.T) {
	good := DaemonDirs{Part: "roots/conf-0a1b2c3d-dev/part", Home: "data/home/.config/caboose", Outside: "canary/outside-root",
		Creds: "canary/creds", Daemon: "home/daemon", Env: "home/envs/cf-0a1b2c3d",
		Checkpoints: "home/envs/cf-0a1b2c3d/checkpoints"}
	envHome := good
	envHome.Home = good.Env + "/data/home/.config/caboose"
	if _, err := DaemonLayout(envHome); err != nil {
		t.Errorf("the environment's default data dir: %v", err)
	}
	specs, err := DaemonLayout(good)
	if err != nil || len(specs) != 9 {
		t.Fatalf("%v, %d canaries", err, len(specs))
	}
	roles := map[string]string{}
	for _, s := range specs {
		roles[s.Role] = s.Parent
	}
	for role, parent := range map[string]string{RoleDHome: good.Home, RoleX1: good.Home, RoleDRoot: good.Part,
		RoleDDaemon: good.Daemon, RoleDGrants: good.Env, RoleDCfg: good.Env, RoleDCkpt: good.Checkpoints,
		RoleDOutside: good.Outside, RoleDCred: good.Creds} {
		if roles[role] != parent {
			t.Errorf("%s in %q, want %q", role, roles[role], parent)
		}
	}
	for name, mod := range map[string]func(*DaemonDirs){
		"no part":          func(d *DaemonDirs) { d.Part = "" },
		"part not a part":  func(d *DaemonDirs) { d.Part = "roots/conf-0a1b2c3d-dev" },
		"home elsewhere":   func(d *DaemonDirs) { d.Home = "data" },
		"home not kept":    func(d *DaemonDirs) { d.Home = "data/home" },
		"another env's":    func(d *DaemonDirs) { d.Home = "home/envs/x/data/home/.config/caboose" },
		"absolute":         func(d *DaemonDirs) { d.Outside = "/tmp/x" },
		"up":               func(d *DaemonDirs) { d.Creds = "../creds" },
		"unclean":          func(d *DaemonDirs) { d.Creds = "canary//creds" },
		"daemon elsewhere": func(d *DaemonDirs) { d.Daemon = "data/daemon" },
		"env elsewhere":    func(d *DaemonDirs) { d.Env = "envs/x" },
		"store elsewhere":  func(d *DaemonDirs) { d.Checkpoints = "home/envs/other/checkpoints" },
	} {
		d := good
		mod(&d)
		if _, err := DaemonLayout(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestProcNetUnixPath(t *testing.T) {
	for l, want := range map[string]string{
		"Num       RefCount Protocol Flags    Type St Inode Path":                      "",
		"0000000000000000: 00000002 00000000 00010000 0001 01 12345 /run/a.sock":       "/run/a.sock",
		"0000000000000000: 00000002 00000000 00010000 0001 01 12346":                   "",
		"0000000000000000: 00000002 00000000 00010000 0001 01 12347 @abstract":         "",
		"0000000000000000: 00000002 00000000 00010000 0001 01 12347 /tmp/x y":          "",
		"0000000000000000: 00000002 00000000 00010000 0001 01 12348 /tmp/tmux/default": "/tmp/tmux/default",
	} {
		if got := procNetUnixPath(l); got != want {
			t.Errorf("%q: %q, want %q", l, got, want)
		}
	}
}

func TestCovers(t *testing.T) {
	for _, x := range []struct {
		a, b string
		want bool
	}{
		{"<B>/home/daemon", "<B>/home/daemon", true}, {"<B>/home", "<B>/home/daemon", true},
		{"<B>", "<B>/home/daemon", true}, {"<B>/home/daemon-dev", "<B>/home/daemon", false},
		{"<B>/home/daemon/x", "<B>/home/daemon", false}, {"", "<B>/home/daemon", false},
		{"<B>/data/home", "<B>/home/daemon", false},
	} {
		if got := covers(x.a, x.b); got != x.want {
			t.Errorf("covers(%q, %q) = %v", x.a, x.b, got)
		}
	}
}

// A real run's roots hold some hundreds of the run's canaries, and every
// path below them carries the run's names in a component (the roots are
// named so): the scans must go through on that many, and say so when
// they find more than they may print, never that they did not go through.
func TestDaemonScansAtRealSize(t *testing.T) {
	w := hostWorld{"container", c.TrustFallible}
	d := newDaemon(t, w)
	for i := 0; i < 600; i++ {
		if _, err := d.MakeCanary(d.Part(), fmt.Sprintf("many-%d", i), c.File); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := d.MakeCanary(d.Part(), "many-repo", c.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AddPlainFiles(repo.Rel, 600); err != nil {
		t.Fatal(err)
	}
	got, _ := daemonRunIn(t, d, d.Env(w.iso, w.trust), []c.Check{daemonByID("D0"), daemonByID("D1")})
	for _, id := range []string{"D0", "D1"} {
		if got[id].Status != c.Pass {
			t.Errorf("%s with 1200 more paths: %s %s", id, got[id].Status, got[id].Reason)
		}
	}

	d = newDaemon(t, w)
	for i := 0; i <= c.ScanMax; i++ {
		if _, err := d.MakeCanary(d.Part(), fmt.Sprintf("many-%d", i), c.File); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = daemonRunIn(t, d, d.Env(w.iso, w.trust), []c.Check{daemonByID("D0")})
	if r := got["D0"]; r.Status != c.Error || !strings.Contains(r.Reason, "more than the 4000 paths") {
		t.Errorf("D0 past the cap: %s %s", r.Status, r.Reason)
	}
}

// Under vm the guest serves sockets of its own: dockerd's answers the
// daemon's hello in HTTP, containerd's ttrpc takes the connection and says
// nothing. D2 notes each as the sandbox's own and passes, and still fails
// on the daemon's socket beside them, on a socket the guest does not bind,
// and on one it binds that answers the daemon's hello. dockerd binds at
// /var/run/docker.sock, which /proc/net/unix names, and the search finds
// it at /run/docker.sock: the same file, by device and inode.
func TestD2GuestSocketIsNotTheDaemon(t *testing.T) {
	for _, w := range []hostWorld{{"vm", c.TrustFallible}, {"vm", c.TrustHostile}} {
		d := newDaemon(t, w)
		res, col := daemonRunIn(t, d, d.Env(w.iso, w.trust), []c.Check{daemonByID("D2")})
		got, lines := res["D2"], col.Lines("D2")
		for _, want := range [][2]string{
			{"/run/docker.sock: the sandbox's own, not the daemon (foreign: ", ", bound as /var/run/docker.sock"},
			{"/run/docker/containerd/containerd.sock.ttrpc: the sandbox's own, not the daemon (closed: connected; read unix @->/var/run/docker/containerd/containerd.sock.ttrpc: i/o timeout)", ", bound as /var/run/docker/containerd/containerd.sock.ttrpc; the daemon's log recorded no connection"},
			// caboose-agent's relay to the host's SSH agent is the host's,
			// never the sandbox's own: named, and passed only on the
			// daemon's log.
			{"/ssh-agent.sock: caboose's relay to the host (the host's SSH agent), not the daemon (closed: connected; EOF)", "; the daemon's log recorded no connection"},
		} {
			if got.Status != c.Pass || !slices.ContainsFunc(lines, func(l string) bool {
				return strings.HasPrefix(l, want[0]) && strings.HasSuffix(l, want[1])
			}) {
				t.Errorf("%v: %s (%s) %q, want a line %q...%q", w, got.Status, got.Reason, lines, want[0], want[1])
			}
		}
		if slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "/ssh-agent.sock: the sandbox's own") }) {
			t.Errorf("%v: the SSH agent's relay called the sandbox's own: %q", w, lines)
		}
		for _, m := range []fake.Mutant{fake.SocketReachable, fake.DaemonDirMounted, fake.ForeignSocketMounted, fake.BoundSocketIsDaemon, fake.SilentRelayToDaemon, fake.AgentRelayToDaemon} {
			if got := daemonOne(t, w, daemonByID("D2"), m); got.Status != c.Fail {
				t.Errorf("%v %q: %s (%s)", w, m, got.Status, got.Reason)
			}
		}
		if got := daemonOne(t, w, daemonByID("D2"), fake.BoundSocketIsDaemon); !strings.Contains(got.Reason, "/run/guest-relay.sock took a connection (accepted: ") {
			t.Errorf("%v: a bound socket that answers the daemon: %s (%s)", w, got.Status, got.Reason)
		}
		// A relay that says nothing is told by the daemon's log alone.
		for m, sock := range map[fake.Mutant]string{fake.SilentRelayToDaemon: "/run/guest-silent-relay.sock", fake.AgentRelayToDaemon: "/run/conf-agent-relay.sock", fake.HelloRelayDropsReply: "/run/guest-drop-relay.sock"} {
			if got := daemonOne(t, w, daemonByID("D2"), m); !strings.Contains(got.Reason, "the socket "+sock+" took a connection (closed: connected; EOF), and the daemon's log recorded ") || !strings.HasSuffix(got.Reason, " connection(s) it refused meanwhile: it reaches the daemon") {
				t.Errorf("%v %q: %s (%s)", w, m, got.Status, got.Reason)
			}
		}
		for f, want := range map[fake.Fault]string{
			fake.WitnessBlind:    "positive control failed: the daemon's log did not record",
			fake.HoldersUnlisted: "positive control failed: the link's socket /run/conf-link-fake.sock was not found held by caboose-agent",
			fake.HoldersCutShort: "the list of the sockets' processes did not go through (find's exit \"124\")",
			fake.LogRotates:      "the daemon's log was moved aside while /run/docker/containerd/containerd.sock.ttrpc was tried",
		} {
			d = newDaemon(t, w)
			d.SetFault(f)
			res, _ = daemonRunIn(t, d, d.Env(w.iso, w.trust), []c.Check{daemonByID("D2")})
			if r := res["D2"]; r.Status != c.Error || !strings.Contains(r.Reason, want) {
				t.Errorf("%v %q: %s (%s)", w, f, r.Status, r.Reason)
			}
		}
		d = newDaemon(t, w)
		d.SetFault(fake.SocketVanishes)
		res, _ = daemonRunIn(t, d, d.Env(w.iso, w.trust), []c.Check{daemonByID("D2")})
		if r := res["D2"]; r.Status != c.Error || !strings.Contains(r.Reason, "was gone by the time it was compared") {
			t.Errorf("%v: a socket gone before the comparison: %s (%s)", w, r.Status, r.Reason)
		}
	}
}

// D2's control finds caboose-agent holding the link's socket wherever
// the agent runs from under each isolation: the layer's path in a
// container or gVisor sandbox, the guest init's copy under vm, either
// with its file deleted. A holder of the agent's name at another path is
// not the agent, and the ERROR says what does hold the socket.
func TestD2KnowsTheAgentUnderEveryIsolation(t *testing.T) {
	for _, w := range daemonWorlds {
		for _, f := range []fake.Fault{"", fake.AgentExeDeleted} {
			d := newDaemon(t, w)
			if f != "" {
				d.SetFault(f)
			}
			res, _ := daemonRunIn(t, d, d.Env(w.iso, w.trust), []c.Check{daemonByID("D2")})
			if r := res["D2"]; r.Status != c.Pass {
				t.Errorf("%v %q: %s (%s)", w, f, r.Status, r.Reason)
			}
		}
		// Only a world with sockets of the sandbox's own that answer (the
		// vm guest's dockerd and containerd) looks at their holders.
		if w.iso != "vm" {
			continue
		}
		d := newDaemon(t, w)
		d.SetFault(fake.AgentExeImpostor)
		res, _ := daemonRunIn(t, d, d.Env(w.iso, w.trust), []c.Check{daemonByID("D2")})
		want := `was not found held by caboose-agent (/usr/local/bin/caboose-agent or /run/caboose/agent), so a relay of its could not be told: held by pid 100 ("/tmp/caboose-agent")`
		if r := res["D2"]; r.Status != c.Error || !strings.HasSuffix(r.Reason, want) {
			t.Errorf("%v: an impostor holds the link: %s (%s)", w, r.Status, r.Reason)
		}
	}
}

func TestIsAgentExe(t *testing.T) {
	for exe, want := range map[string]bool{
		"/usr/local/bin/caboose-agent":           true,
		"/usr/local/bin/caboose-agent (deleted)": true,
		agentproto.GuestAgent:                    true,
		agentproto.GuestAgent + " (deleted)":     true,
		"/tmp/caboose-agent":                     false,
		"/usr/bin/caboose-agent":                 false,
		"/init":                                  false,
		"/agent":                                 false,
		"caboose-agent":                          false,
		"/run/caboose/agent (deleted) (deleted)": false,
		"/run/caboose/agent/":                    false,
		"":                                       false,
	} {
		if got := isAgentExe(exe); got != want {
			t.Errorf("%q: %v, want %v", exe, got, want)
		}
	}
}

// The agent's paths are where the layer and the guest's init put it.
func TestAgentExesAreWhereItIsInstalled(t *testing.T) {
	b, err := os.ReadFile("../../../layer.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^COPY \S+ ` + regexp.QuoteMeta(agentExes[0]) + `$`).Match(b) {
		t.Errorf("layer.Dockerfile does not COPY caboose-agent to %s", agentExes[0])
	}
	b, err = os.ReadFile("../../agent/init_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "const guestAgent = agentproto.GuestAgent") || !strings.Contains(string(b), `copyFile("/init", "/mnt/root"+guestAgent, `) {
		t.Error("the guest's init no longer copies itself to agentproto.GuestAgent")
	}
}

func TestDescribeHoldersIsBounded(t *testing.T) {
	h := &holders{byInode: map[string][]int{}, agent: map[int]bool{}, exe: map[int]string{}}
	for pid := 1; pid <= 12; pid++ {
		h.byInode["7"] = append(h.byInode["7"], pid)
		h.exe[pid] = "/x\u202e" + strings.Repeat("y", 300)
	}
	delete(h.exe, 3)
	got := h.describe([]string{"7"})
	if !strings.Contains(got, "pid 3 (executable unread)") || !strings.HasSuffix(got, "and 4 more") || strings.ContainsRune(got, '\u202e') || len(got) > 8*200 {
		t.Errorf("%d bytes: %q", len(got), got)
	}
	if got := h.describe([]string{"8"}); got != "no process was found holding it" {
		t.Errorf("%q", got)
	}
}

// noDaemonAnswer takes only a connection that got nothing in the daemon's
// protocol: any frame the daemon's reader took is the daemon.
func TestNoDaemonAnswer(t *testing.T) {
	for out, want := range map[string]bool{
		probe.Foreign: true, probe.Closed: true,
		probe.Accepted: false, probe.Refused: false, probe.Failed: false,
		probe.Unreachable: false, probe.NoHost: false, probe.RateLimited: false, probe.Skipped: false,
	} {
		if got := noDaemonAnswer(probe.Outcome{Outcome: out}); got != want {
			t.Errorf("%s: %v, want %v", out, got, want)
		}
	}
}
