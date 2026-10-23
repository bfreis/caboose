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

// The worlds a sandbox exists in: hostile without vm runs T1 alone.
var ckptWorlds = []hostWorld{
	{"container", c.TrustFallible}, {"gvisor", c.TrustFallible}, {"vm", c.TrustFallible}, {"vm", c.TrustHostile},
}

func newCkpt(t testing.TB, w hostWorld) *fake.Checkpoints {
	t.Helper()
	k, err := fake.NewCheckpoints(hostN8, w.iso, w.trust)
	if err != nil {
		t.Fatal(err)
	}
	fake.NoAgentRuns(t, k.Fake)
	layout, err := CheckpointLayout(CheckpointDirs{
		Part: k.Part(), Canary: "canary", Store: fake.CkptStore, EnvDir: fake.CkptEnvDir,
		Home: fake.CkptHome, DataKeep: fake.CkptDataKeep,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, hc := range layout {
		if _, err := k.MakeCanary(hc.Parent, hc.Role, hc.Kind); err != nil {
			t.Fatal(err)
		}
	}
	return k
}

// ckptAnchor is a harmless first step of the group, so a destructive check
// can run alone.
func ckptAnchor() c.Check {
	return c.Check{ID: "anchor", Group: groupCheckpoints, Class: c.Observe,
		Run: func(context.Context, c.Env) c.CheckResult { return c.CheckResult{Status: c.Pass} }}
}

// ckptRunIn runs the checks through the runner and its envelope. A
// destructive FAIL ends a run, which is not a test failure; touching what
// a check was not handed is.
func ckptRunIn(t testing.TB, k *fake.Checkpoints, env c.Env, checks []c.Check) map[string]c.Result {
	t.Helper()
	r := &c.Runner{Env: &fake.Envelope{F: k.Fake}}
	res, _, err := c.RunChecks(context.Background(), r, append([]c.Check{ckptAnchor()}, checks...), env)
	var te *c.TouchedError
	if errors.As(err, &te) {
		t.Errorf("run touched what it was not handed: %v", err)
	}
	out := map[string]c.Result{}
	for _, x := range res {
		out[x.ID] = x
	}
	return out
}

func ckptOne(t testing.TB, w hostWorld, ck c.Check, set func(*fake.Checkpoints)) c.Result {
	t.Helper()
	k := newCkpt(t, w)
	if set != nil {
		set(k)
	}
	return ckptRunIn(t, k, k.Env(w.iso, w.trust), []c.Check{ck})[ck.ID]
}

func TestCheckpointEnforcing(t *testing.T) {
	for _, w := range ckptWorlds {
		k := newCkpt(t, w)
		got := ckptRunIn(t, k, k.Env(w.iso, w.trust), CheckpointChecks())
		for _, ck := range CheckpointChecks() {
			if r := got[ck.ID]; r.Status != c.Pass {
				t.Errorf("%v %s: %s %s, want PASS", w, ck.ID, r.Status, r.Reason)
			}
		}
		if err := fake.CheckOrder(k.Events()); err != nil {
			t.Errorf("%v: %v", w, err)
		}
		if len(k.Foreign) != 0 {
			t.Errorf("%v: restores to %v", w, k.Foreign)
		}
	}
}

func TestCheckpointChecksList(t *testing.T) {
	cs := CheckpointChecks()
	var ids []string
	for _, ck := range cs {
		ids = append(ids, ck.ID)
		if ck.Group != groupCheckpoints || ck.Wipe || ck.Fresh || ck.Title == "" || ck.Control == "" {
			t.Errorf("%s: %+v", ck.ID, ck)
		}
	}
	if !slices.Equal(ids, []string{"C1", "C2", "C3", "C4", "C5", "C6"}) {
		t.Errorf("ids %v", ids)
	}
	classes := map[string]c.Class{"C1": c.Observe, "C2": c.Destructive, "C3": c.Destructive, "C4": c.Write, "C5": c.Destructive, "C6": c.Destructive}
	for _, ck := range cs {
		if ck.Class != classes[ck.ID] {
			t.Errorf("%s is class %v", ck.ID, ck.Class)
		}
	}
	layout, err := CheckpointLayout(CheckpointDirs{"roots/p/part", "canary", "home/envs/e/checkpoints", "home/envs/e", "home", "data/home/.config/caboose"})
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]bool{}
	for _, hc := range layout {
		roles[hc.Role] = true
	}
	for _, ck := range cs {
		for _, r := range ck.Roles {
			if !roles[r] {
				t.Errorf("%s changes %s, which is no canary of the layout", ck.ID, r)
			}
		}
	}
}

func TestCheckpointLayout(t *testing.T) {
	good := CheckpointDirs{"roots/p/part", "canary", "home/envs/e/checkpoints", "home/envs/e", "home", "data/home/.config/caboose"}
	if _, err := CheckpointLayout(good); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func(*CheckpointDirs){
		func(d *CheckpointDirs) { d.Part = "" },
		func(d *CheckpointDirs) { d.Canary = "/tmp/canary" },
		func(d *CheckpointDirs) { d.Home = "../home" },
		func(d *CheckpointDirs) { d.Store = "home/envs/other/checkpoints" },
		func(d *CheckpointDirs) { d.EnvDir, d.Store = "elsewhere/e", "elsewhere/e/checkpoints" },
		func(d *CheckpointDirs) { d.DataKeep = "data/./home" },
	} {
		d := good
		f(&d)
		if _, err := CheckpointLayout(d); err == nil {
			t.Errorf("%+v accepted", d)
		}
	}
}

// ckptTable is what each mutant makes of each check, each alone in a
// fresh model; a check not named keeps its enforcing status.
var ckptTable = map[fake.Mutant]map[string]c.Status{
	fake.CheckpointStoreMounted: {"C1": c.Fail},
	fake.StoreParentMounted:     {"C1": c.Fail},
	fake.StoreChildMounted:      {"C1": c.Fail},
	fake.StoreSeen:              {"C1": c.Fail},
	fake.CheckpointAliasesLive:  {"C2": c.Fail, "C3": c.Fail},
	fake.RestoreInPlace:         {"C3": c.Fail},
	fake.RestoreWrong:           {"C3": c.Fail},
	fake.RestoreRefused:         {"C3": c.Fail, "C4": c.Error},
	fake.RestoreFollowsLink:     {"C3": c.Fail},
	fake.RestoreUnsafeSilent:    {"C4": c.Fail},
	fake.RestoreUnsafeWarned:    {"C4": c.Fail},
	fake.ShrinkUnflagged:        {"C5": c.Fail},
	fake.PruneOutsideStore:      {"C6": c.Fail},
	fake.PruneIgnoresPins:       {"C6": c.Fail},
	fake.PruneDropsNewest:       {"C6": c.Fail},
	fake.ShrinkUnpinned:         {"C5": c.Fail},
}

func TestCheckpointMutants(t *testing.T) {
	for _, m := range fake.CheckpointMutants {
		if _, ok := ckptTable[m]; !ok {
			t.Errorf("mutant %q has no row", m)
		}
	}
	failed := map[fake.Mutant]bool{}
	covered := map[string]bool{}
	for _, w := range ckptWorlds {
		for _, m := range fake.CheckpointMutants {
			for _, ck := range CheckpointChecks() {
				want, ok := ckptTable[m][ck.ID]
				if !ok {
					want = c.Pass
				}
				got := ckptOne(t, w, ck, func(k *fake.Checkpoints) { k.Break(m) })
				if got.Status != want {
					t.Errorf("%v %s under %q: %s (%s), want %s", w, ck.ID, m, got.Status, got.Reason, want)
				}
				if got.Status == c.Fail {
					failed[m] = true
					covered[ck.ID] = true
				}
			}
		}
	}
	for _, m := range fake.CheckpointMutants {
		if !failed[m] {
			t.Errorf("mutant %q fails nothing", m)
		}
	}
	for _, ck := range CheckpointChecks() {
		if !covered[ck.ID] {
			t.Errorf("%s is failed by no mutant", ck.ID)
		}
	}
}

// ckptFaults are what each fault makes of the checks: ERROR, never PASS,
// but where the product's own answer is a failure.
var ckptFaults = map[fake.CkptFault]map[string]c.Status{
	fake.StoreUnknown:     {"C1": c.Error},
	fake.MountsUnlisted:   {"C1": c.Error},
	fake.ScanBlind:        {"C1": c.Error},
	fake.CreateFails:      {"C2": c.Error, "C3": c.Error, "C4": c.Error, "C5": c.Error, "C6": c.Error},
	fake.TreeUnreadable:   {"C2": c.Error, "C3": c.Error},
	fake.CopyIncomplete:   {"C2": c.Error, "C3": c.Error},
	fake.DamageIgnored:    {"C2": c.Error, "C3": c.Error, "C5": c.Error},
	fake.LinkNotMade:      {"C3": c.Error},
	fake.RestoreErrors:    {"C3": c.Error, "C4": c.Error},
	fake.ControlWarned:    {"C4": c.Error},
	fake.PopulateFails:    {"C5": c.Error},
	fake.ShrinkTooSmall:   {"C5": c.Error},
	fake.PruneNothing:     {"C6": c.Error},
	fake.PinnedVanishes:   {"C6": c.Error},
	fake.ListFails:        {"C5": c.Error, "C6": c.Error},
	fake.StubsGone:        {"C1": c.Error, "C2": c.Error, "C3": c.Error, "C4": c.Error, "C5": c.Error, "C6": c.Error},
	fake.RestoreAsExists:  {"C3": c.Fail, "C4": c.Error},
	fake.UnsafeAsExisting: {"C4": c.Error},
	fake.DiscardFails:     {"C4": c.Error},
}

func TestCheckpointFaults(t *testing.T) {
	for _, f := range fake.CkptFaults {
		if _, ok := ckptFaults[f]; !ok {
			t.Errorf("fault %q has no row", f)
		}
	}
	for _, w := range ckptWorlds {
		for _, f := range fake.CkptFaults {
			for _, ck := range CheckpointChecks() {
				want, ok := ckptFaults[f][ck.ID]
				if !ok {
					want = c.Pass
				}
				got := ckptOne(t, w, ck, func(k *fake.Checkpoints) { k.SetCkptFault(f) })
				if got.Status != want {
					t.Errorf("%v %s with %q: %s (%s), want %s", w, ck.ID, f, got.Status, got.Reason, want)
				}
			}
		}
	}
}

// A model whose sandbox, shell or canaries are missing, or a host without
// the calls, gives ERROR, never PASS.
func TestCheckpointNoVacuousPass(t *testing.T) {
	w := ckptWorlds[0]
	faults := map[string]func(*fake.Checkpoints){
		"sandbox down":  func(k *fake.Checkpoints) { k.SetDown(true) },
		"shell missing": func(k *fake.Checkpoints) { k.SetShellMissing(true) },
		"stubs missing": func(k *fake.Checkpoints) { k.SetStubsMissing(true) },
		"part refuses":  func(k *fake.Checkpoints) { k.SetPartReadOnly(true) },
		"seen missing":  func(k *fake.Checkpoints) { k.HideCanary(RoleSeen) },
	}
	for name, set := range faults {
		for _, ck := range CheckpointChecks() {
			if (name == "shell missing" || name == "part refuses") && (ck.ID == "C4" || ck.ID == "C6") {
				continue // they send nothing to the sandbox
			}
			if name == "part refuses" && ck.ID == "C1" {
				continue // it reads only
			}
			if got := ckptOne(t, w, ck, set); got.Status != c.Error {
				t.Errorf("%s with %s: %s (%s), want ERROR", ck.ID, name, got.Status, got.Reason)
			}
		}
	}
	for _, ck := range CheckpointChecks() {
		for _, role := range ck.Roles {
			if got := ckptOne(t, w, ck, func(k *fake.Checkpoints) { k.HideCanary(role) }); got.Status != c.Error {
				t.Errorf("%s with %s missing: %s (%s), want ERROR", ck.ID, role, got.Status, got.Reason)
			}
		}
		// A host without the checkpoint calls, or without the host calls.
		k := newCkpt(t, w)
		if got := ckptRunIn(t, k, c.NewEnv(k, k.Link, k, w.iso, w.trust), []c.Check{ck})[ck.ID]; got.Status != c.Error {
			t.Errorf("%s with no checkpoint calls: %s (%s)", ck.ID, got.Status, got.Reason)
		}
		k = newCkpt(t, w)
		if got := ckptRunIn(t, k, c.NewEnv(k, k.Fake, k, w.iso, w.trust), []c.Check{ck})[ck.ID]; got.Status != c.Error {
			t.Errorf("%s with no host calls: %s (%s)", ck.ID, got.Status, got.Reason)
		}
	}
}

// Every restore any check asks for, under every mutant, is into a canary
// of the run; and the model refuses one that is not.
func TestCheckpointRestoreTargetsAreTheSuites(t *testing.T) {
	for _, w := range ckptWorlds {
		for _, m := range append([]fake.Mutant{fake.Enforcing}, fake.CheckpointMutants...) {
			k := newCkpt(t, w)
			k.Break(m)
			ckptRunIn(t, k, k.Env(w.iso, w.trust), CheckpointChecks())
			if len(k.Targets) == 0 && m == fake.Enforcing {
				t.Fatalf("%v: no restore asked for", w)
			}
			canaries := k.CanaryPaths()
			for _, tg := range k.Targets {
				in := false
				for _, p := range canaries {
					in = in || tg == p || strings.HasPrefix(tg, p+"/")
				}
				if !in || !strings.HasPrefix(tg, "canary/") && !strings.HasPrefix(tg, k.Part()+"/") &&
					!strings.HasPrefix(tg, "home/") && !strings.HasPrefix(tg, fake.CkptDataKeep+"/") {
					t.Errorf("%v %q: a restore to %s", w, m, tg)
				}
			}
			if len(k.Foreign) != 0 {
				t.Errorf("%v %q: foreign targets %v", w, m, k.Foreign)
			}
		}
	}
	k := newCkpt(t, ckptWorlds[0])
	if _, err := k.Restore(context.Background(), "x", c.RestoreTarget{}); err == nil || len(k.Foreign) != 1 {
		t.Error("a target no constructor made was taken")
	}
}

// recordingCkpt keeps every command it is sent.
type recordingCkpt struct {
	*fake.Checkpoints
	mu   sync.Mutex
	sent [][]string
}

func (r *recordingCkpt) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	r.mu.Lock()
	r.sent = append(r.sent, slices.Clone(argv))
	r.mu.Unlock()
	return r.Checkpoints.Exec(ctx, user, argv, stdin)
}

var (
	ckptPath = `/work/conf-[0-9a-f]{8}-dev/part/conf-[0-9a-f]{8}-[a-z0-9-]+(/[a-z0-9][a-z0-9.-]*)*`
	ckptForm = regexp.MustCompile(`^((echo x >>|truncate -s 0|rm|rm -r|touch) ` + ckptPath +
		`|(mv|ln -s) ` + ckptPath + ` ` + ckptPath +
		`|timeout 60 find / -path /proc -prune -o -path /sys -prune -o -path /dev -prune( -o -name 'conf-[0-9a-f]{8}-[a-z0-9-]+' -print)+ 1>&2)$`)
)

// The commands are literal: the forms above, only canary paths of the
// writable part (the scan aside), no expansion, glob or tilde, valid sh.
func TestCheckpointCommandsAreLiteral(t *testing.T) {
	sent := map[string]bool{}
	for _, w := range ckptWorlds {
		for _, m := range append([]fake.Mutant{fake.Enforcing}, fake.CheckpointMutants...) {
			k := newCkpt(t, w)
			k.Break(m)
			r := &recordingCkpt{Checkpoints: k}
			ckptRunIn(t, k, c.NewEnv(r, r, r, w.iso, w.trust), CheckpointChecks())
			for _, argv := range r.sent {
				body, ok := c.Unwrap(argv)
				if !ok {
					t.Fatalf("not the wrapper: %q", argv)
				}
				sent[body] = true
			}
		}
	}
	if len(sent) < 10 {
		t.Fatalf("only %d distinct commands", len(sent))
	}
	unquoted := regexp.MustCompile(`'[^']*'`)
	for body := range sent {
		if !ckptForm.MatchString(body) {
			t.Errorf("not a form the checkpoint checks send: %q", body)
		}
		if strings.ContainsAny(body, "$`~") || strings.ContainsAny(unquoted.ReplaceAllString(body, "''"), "*?[]") {
			t.Errorf("expansion, glob or tilde: %q", body)
		}
		if out, err := exec.Command("sh", "-n", "-c", "LC_ALL=C; { "+body+"; } >/dev/null").CombinedOutput(); err != nil {
			t.Errorf("sh -n rejected %q: %v %s", body, err, out)
		}
	}
}

// With every mutant on at once, the model changes nothing but canaries.
func TestCheckpointTouchesOnlyCanaries(t *testing.T) {
	for _, w := range ckptWorlds {
		k := newCkpt(t, w)
		for _, m := range fake.CheckpointMutants {
			k.Break(m)
		}
		ckptRunIn(t, k, k.Env(w.iso, w.trust), CheckpointChecks())
		canaries := k.CanaryPaths()
		for _, mu := range k.Mutations() {
			ok := false
			for _, p := range canaries {
				ok = ok || mu.Rel == p || strings.HasPrefix(mu.Rel, p+"/")
			}
			if !ok {
				t.Errorf("%v: %s %s is no canary's", w, mu.Op, mu.Rel)
			}
		}
	}
}

// The check file runs nothing itself and names nothing the suite never
// opens.
func TestCheckpointSourceIsInert(t *testing.T) {
	banned := map[string]bool{"os": true, "os/exec": true, "net": true, "syscall": true, "golang.org/x/sys/unix": true, "plugin": true, "unsafe": true}
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, "checkpoint.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range f.Imports {
		if p, _ := strconv.Unquote(im.Path.Value); banned[p] {
			t.Errorf("checkpoint.go imports %s", p)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			s, _ := strconv.Unquote(x.Value)
			for _, bad := range []string{"claude", ".ssh", ".credentials", "ssh_auth_sock", "/users/", "$home", "keychain", "caboose-agent"} {
				if strings.Contains(strings.ToLower(s), bad) {
					t.Errorf("checkpoint.go:%d: a string naming %q", fs.Position(x.Pos()).Line, bad)
				}
			}
		case *ast.SelectorExpr:
			// The removal builders only through their Op, so a scan for
			// removal calls stays meaningful.
			if x.Sel.Name == "Remove" || x.Sel.Name == "RemoveTree" || x.Sel.Name == "RemoveAll" {
				t.Errorf("checkpoint.go:%d: calls %s", fs.Position(x.Pos()).Line, x.Sel.Name)
			}
		}
		return true
	})
}

// C4 restores into places the sandbox sees, the data dir's kept home among
// them, and the copy holds the run's names and nonce: left there, the
// daemon checks' scans after it find the suite's files where no mount of
// the suite's puts them (D0) and the nonce where the suite shared nothing
// (D1). C4 removes every copy it asked for once its verdict is read, so
// D0 and D1, run after it in the same world, still pass with no
// exemption for it.
func TestC4LeavesNothingForTheScans(t *testing.T) {
	for _, w := range ckptWorlds {
		// The product refuses; a product that only warns writes into the
		// data dir's kept home, which is what the scans would find.
		for m, want := range map[fake.Mutant]c.Status{"": c.Pass, fake.RestoreUnsafeWarned: c.Fail} {
			testC4Leaves(t, w, m, want)
		}
	}
}

func testC4Leaves(t *testing.T, w hostWorld, m fake.Mutant, want c.Status) {
	t.Helper()
	k := newCkpt(t, w)
	if m != "" {
		k.Break(m)
	}
	got := ckptRunIn(t, k, k.Env(w.iso, w.trust), []c.Check{c4()})
	if r := got["C4"]; r.Status != want {
		t.Fatalf("%v %q C4: %s %s", w, m, r.Status, r.Reason)
	}
	if len(k.Discarded) != 4 {
		t.Errorf("%v: discarded %v, want the control and the three targets", w, k.Discarded)
	}
	d, dirs := fake.DaemonOn(k.Link)
	specs, err := DaemonLayout(DaemonDirs(dirs))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if _, err := d.MakeCanary(s.Parent, s.Role, s.Kind); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := daemonRunIn(t, d, d.Env(w.iso, w.trust), []c.Check{daemonByID("D0"), daemonByID("D1")})
	for _, id := range []string{"D0", "D1"} {
		if r := res[id]; r.Status != c.Pass {
			t.Errorf("%v %s after C4: %s %s", w, id, r.Status, r.Reason)
		}
	}
}
