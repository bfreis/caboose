package conformance_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/fake"
)

func TestEnforcingRun(t *testing.T) {
	for _, iso := range []string{isoContainer, "gvisor", isoVM} {
		f, env := newWorld(t, iso)
		res, col, err := runAll(t, f, env, allChecks(), iso, true)
		if err != nil {
			t.Fatalf("%s: %v", iso, err)
		}
		for _, r := range res {
			want := c.Pass
			if r.ID == "G1" {
				want = c.Gap
			}
			if r.Status != want {
				t.Errorf("%s %s: %s %s", iso, r.ID, r.Status, r.Reason)
			}
		}
		if got, _ := statusOf(res, "V1"); (iso == isoVM) != (got == c.Pass) {
			t.Errorf("%s: V1 is %q", iso, got)
		}
		if got, _ := statusOf(res, "G1"); (iso == isoVM) != (got == "") {
			t.Errorf("%s: G1 is %q", iso, got)
		}
		if err := fake.CheckOrder(f.Events()); err != nil {
			t.Errorf("%s: %v", iso, err)
		}
		if env.DestructiveVerifies != 3 {
			t.Errorf("%s: %d destructive verifies, want one per destructive step (3)", iso, env.DestructiveVerifies)
		}
		_ = col
	}
}

func TestTrustSelectsChecks(t *testing.T) {
	f, env := newWorld(t, isoContainer)
	only := c.Check{ID: "H1", Group: "g", Class: c.Observe, Trusts: []string{"hostile"},
		Run: func(context.Context, c.Env) c.CheckResult { return c.CheckResult{Status: c.Pass} }}
	res, _, err := runAll(t, f, env, []c.Check{only}, isoContainer, true)
	if err != nil || len(res) != 0 {
		t.Fatalf("fallible ran a hostile-only check: %v %v", res, err)
	}
}

func TestMutantPerControl(t *testing.T) {
	fake.CheckMutants(t, allChecks(), table, fake.Config{Isolation: isoContainer, Trust: "fallible", Setup: setup})
}

// recorder collects what CheckMutants reports.
type recorder struct {
	testing.TB
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}
func (r *recorder) Helper() {}

func TestCheckMutantsCatches(t *testing.T) {
	cfg := fake.Config{Isolation: isoContainer, Trust: "fallible", Setup: setup}
	run := func(tb fake.Table) []string {
		r := &recorder{TB: t}
		fake.CheckMutants(r, allChecks(), tb, cfg)
		return r.errs
	}
	if errs := run(table); len(errs) != 0 {
		t.Fatalf("the real table complained: %v", errs)
	}
	clone := func() fake.Table {
		o := fake.Table{}
		for k, v := range table {
			o[k] = slices.Clone(v)
		}
		return o
	}
	// A check with no mutant.
	missing := clone()
	missing[fake.ParentWritable] = []string{"F7"}
	if errs := run(missing); !containsAny(errs, "F2") {
		t.Errorf("a check with no mutant not caught: %v", errs)
	}
	// A mutant that fails nothing.
	nothing := clone()
	nothing[fake.DaemonDirMounted] = nil
	if errs := run(nothing); !containsAny(errs, "fails nothing") {
		t.Errorf("a mutant failing nothing not caught: %v", errs)
	}
	// A mutant that fails a check not assigned to it.
	over := clone()
	over[fake.RootsWritable] = []string{"F1", "F3", "F4", "F7"}
	if errs := run(over); !containsAny(errs, "F6") {
		t.Errorf("an unassigned failure not caught: %v", errs)
	}
	// An assigned check that does not fail.
	wrong := clone()
	wrong[fake.SymlinkFollowed] = []string{"F4", "F1"}
	if errs := run(wrong); !containsAny(errs, "F1") {
		t.Errorf("an assigned check that does not fail not caught: %v", errs)
	}
}

func containsAny(errs []string, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}

func TestNoVacuousPass(t *testing.T) {
	cases := map[string]func(f *fake.Fake){
		"sandbox down":           func(f *fake.Fake) { f.SetDown(true) },
		"target canary missing":  func(f *fake.Fake) { f.HideCanary("ro-file") },
		"allowed canary missing": func(f *fake.Fake) { f.HideCanary("wr-file") },
		"shell missing":          func(f *fake.Fake) { f.SetShellMissing(true) },
		"positive control fails": func(f *fake.Fake) { f.SetPartReadOnly(true) },
	}
	for name, mod := range cases {
		f, env := newWorld(t, isoContainer)
		mod(f)
		res, _, err := runAll(t, f, env, refusals()[:1], isoContainer, true)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st, why := statusOf(res, "F1"); st != c.Error {
			t.Errorf("%s: %s (%s), want ERROR", name, st, why)
		}
	}
	// An ERROR in a non-destructive check stops the destructive ones.
	f, env := newWorld(t, isoContainer)
	f.SetPartReadOnly(true)
	res, _, _ := runAll(t, f, env, append(refusals(), wipe()), isoContainer, true)
	for _, id := range []string{"F5", "F6", "F7"} {
		if st, _ := statusOf(res, id); st != c.Skip {
			t.Errorf("%s after an ERROR: %s, want SKIP", id, st)
		}
	}
	if env.DestructiveVerifies != 0 {
		t.Error("a destructive verify ran after an ERROR")
	}
}

// A check cannot run outside the runner: its Env does nothing.
func TestCheckCannotRunWithoutEnvelope(t *testing.T) {
	f, _ := newWorld(t, isoContainer)
	for _, ck := range refusals() {
		res := ck.Run(context.Background(), f.Env(isoContainer, "fallible"))
		if res.Status == c.Pass {
			t.Errorf("%s passed outside the runner", ck.ID)
		}
	}
	if ev := f.Events(); len(ev) != 0 {
		t.Errorf("the sandbox ran something outside the runner: %v", ev)
	}
	if _, err := f.Env(isoContainer, "fallible").Wipe(context.Background()); err == nil {
		t.Error("Wipe worked outside the runner")
	}
}

func TestWipeAndDestructiveGates(t *testing.T) {
	f, env := newWorld(t, isoContainer)
	var errs []error
	bad := func(id string, class c.Class) c.Check {
		return c.Check{ID: id, Group: "g", Class: class, Roles: allRoles,
			Run: func(ctx context.Context, e c.Env) c.CheckResult {
				_, err := e.Wipe(ctx)
				errs = append(errs, err)
				ro, _ := e.Canary("ro-file")
				p, _ := c.SandboxPath(ro)
				rm, _ := c.Remove(p)
				_, err = e.RunCmd(ctx, "agent", rm)
				errs = append(errs, err)
				return c.CheckResult{Status: c.Pass}
			}}
	}
	_, _, err := runAll(t, f, env, []c.Check{bad("W1", c.Observe), bad("W2", c.Write), bad("D1", c.Destructive)}, isoContainer, true)
	if err != nil {
		t.Fatal(err)
	}
	// W1 and W2: wipe and remove refused. D1: wipe refused (not a wipe
	// check), remove allowed.
	for i, e := range errs {
		wantErr := i != 5
		if (e != nil) != wantErr {
			t.Errorf("call %d: err = %v", i, e)
		}
	}
	// A wipe step that is not destructive is refused up front.
	_, err = c.Steps([]c.Check{{ID: "X", Wipe: true, Class: c.Write, Run: wipe().Run}}, f.Env(isoContainer, "fallible"), &c.Collector{})
	if err == nil {
		t.Error("a wipe check that is not destructive was accepted")
	}
}

func TestSkipWipeRemovesOnlyTheWipe(t *testing.T) {
	f1, e1 := newWorld(t, isoContainer)
	on, _, err := runAll(t, f1, e1, allChecks(), isoContainer, true)
	if err != nil {
		t.Fatal(err)
	}
	f2, e2 := newWorld(t, isoContainer)
	off, _, err := runAll(t, f2, e2, allChecks(), isoContainer, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(on) != len(off) {
		t.Fatal("different lengths")
	}
	for i := range on {
		if on[i].ID == "F7" {
			if off[i].Status != c.Skip {
				t.Errorf("wipe with --skip-wipe: %s", off[i].Status)
			}
			continue
		}
		if on[i].Status != off[i].Status {
			t.Errorf("%s changed with --skip-wipe: %s -> %s", on[i].ID, on[i].Status, off[i].Status)
		}
	}
	for _, ev := range f2.Events() {
		if ev == "exec-destructive" {
			break
		}
	}
	if e2.DestructiveVerifies != 2 {
		t.Errorf("destructive verifies with --skip-wipe: %d, want 2", e2.DestructiveVerifies)
	}
}

func TestCheckOrderCatches(t *testing.T) {
	for name, ev := range map[string][]string{
		"exec first":                    {"exec"},
		"destructive without verify":    {"verify", "exec-destructive"},
		"destructive after post":        {"verify", "verify-destructive", "exec-destructive", "post", "exec-destructive"},
		"destructive before its verify": {"verify", "exec-destructive", "verify-destructive"},
	} {
		if fake.CheckOrder(ev) == nil {
			t.Errorf("%s: not caught", name)
		}
	}
	if err := fake.CheckOrder([]string{"verify", "verify-destructive", "exec-destructive", "exec", "post"}); err != nil {
		t.Error(err)
	}
}

// Whatever the mutants and the order, the host is asked to change only
// canaries and what is below them; a stray write ends the run.
func TestTouchesOnlyCanaries(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 60; i++ {
		f, env := newWorld(t, isoContainer)
		for _, m := range fake.All {
			if rng.IntN(2) == 0 {
				f.Break(m)
			}
		}
		if i == 0 {
			f.BreakAll()
		}
		checks := allChecks()
		rng.Shuffle(len(checks), func(a, b int) { checks[a], checks[b] = checks[b], checks[a] })
		res, _, err := runAll(t, f, env, checks, isoContainer, true)
		for _, r := range res {
			// (A shuffled order may run the wipe before a check whose
			// canary it removed.)
			if r.Status == c.Error && !strings.Contains(r.Reason, "touched") && !strings.Contains(r.Reason, "not on the host") {
				t.Fatalf("run %d: %s ERROR %s", i, r.ID, r.Reason)
			}
		}
		_ = err
		canaries := f.CanaryPaths()
		for _, m := range f.Mutations() {
			if !underAny(m.Rel, canaries) {
				t.Fatalf("run %d: %s %s is not a canary", i, m.Op, m.Rel)
			}
		}
		if err := fake.CheckOrder(f.Events()); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

func underAny(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

func TestStrayWriteEndsTheRun(t *testing.T) {
	f, env := newWorld(t, isoContainer)
	f.InjectStray(f.Dev() + "/stray-file")
	res, _, err := runAll(t, f, env, allChecks(), isoContainer, true)
	var te *c.TouchedError
	if !asTouched(err, &te) {
		t.Fatalf("err = %v, want TouchedError", err)
	}
	if st, _ := statusOf(res, "F1"); st != c.Error {
		t.Errorf("the check that was running: %s, want ERROR", st)
	}
	if st, _ := statusOf(res, "F2"); st != c.Skip {
		t.Errorf("the next check: %s, want SKIP", st)
	}
}

func asTouched(err error, out **c.TouchedError) bool {
	te, ok := err.(*c.TouchedError)
	*out = te
	return ok
}
