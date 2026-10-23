package fake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/conformance"
)

// Envelope is a conformance.Envelope over a Fake: it counts the calls,
// records them beside the sandbox's execs, and walks the model after each
// check as the real post-check walks B.
type Envelope struct {
	F *Fake
	// RefuseAt makes the nth Verify (1-based) refuse; 0 never.
	RefuseAt int

	Verifies, DestructiveVerifies int
}

func (e *Envelope) Verify(context.Context) error {
	e.F.mu.Lock()
	defer e.F.mu.Unlock()
	e.Verifies++
	e.F.event("verify")
	if e.RefuseAt != 0 && e.Verifies >= e.RefuseAt {
		return &conformance.RefusalError{Item: "fake", Err: fmt.Errorf("refused")}
	}
	return nil
}

func (e *Envelope) VerifyDestructive(context.Context) error {
	e.F.mu.Lock()
	defer e.F.mu.Unlock()
	e.DestructiveVerifies++
	e.F.event("verify-destructive")
	return nil
}

func (e *Envelope) snapshot() conformance.Snapshot {
	s := conformance.Snapshot{}
	for p, n := range e.F.nodes {
		s[p] = e.F.identity(n)
	}
	return s
}

func (e *Envelope) Snapshot() (conformance.Snapshot, error) {
	e.F.mu.Lock()
	defer e.F.mu.Unlock()
	return e.snapshot(), nil
}

// PostCheck reports any path outside allowed (or below one of them) that
// differs from before.
func (e *Envelope) PostCheck(before conformance.Snapshot, allowed []string) (conformance.Snapshot, error) {
	e.F.mu.Lock()
	defer e.F.mu.Unlock()
	e.F.event("post")
	after := e.snapshot()
	ok := func(p string) bool {
		for _, a := range allowed {
			if p == a || strings.HasPrefix(p, a+"/") {
				return true
			}
		}
		return false
	}
	var changed []string
	for p, b := range before {
		if a, there := after[p]; !there || !sameID(a, b) {
			if !ok(p) {
				changed = append(changed, p)
			}
		}
	}
	for p := range after {
		if _, there := before[p]; !there && !ok(p) {
			changed = append(changed, p)
		}
	}
	slices.Sort(changed)
	if len(changed) > 0 {
		return after, &conformance.TouchedError{Paths: changed}
	}
	e.F.recordAllowed(allowed)
	return after, nil
}

func sameID(a, b conformance.Identity) bool {
	if a.Mode.IsDir() && b.Mode.IsDir() {
		return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode
	}
	return a == b
}

// CheckOrder reports a way the events break the envelope's order: an exec
// before any Verify, or a destructive exec that did not follow a
// VerifyDestructive since the last post-check.
func CheckOrder(events []string) error {
	verified, destructiveOK := false, false
	for i, ev := range events {
		switch ev {
		case "verify":
			verified = true
		case "verify-destructive":
			destructiveOK = true
		case "post":
			destructiveOK = false
		case "exec":
			if !verified {
				return fmt.Errorf("event %d: an exec before any Verify", i)
			}
		case "exec-destructive":
			if !verified || !destructiveOK {
				return fmt.Errorf("event %d: a destructive exec without VerifyDestructive before it", i)
			}
		}
	}
	return nil
}

// Table maps each mutant to the ids of the checks it must fail.
type Table map[Mutant][]string

// Config is how CheckMutants builds a world for each run.
type Config struct {
	Isolation, Trust string
	// AgentWrites is a gVisor engine where the agent can write its
	// mounts, so the sandbox runs as the agent (SetRunsAsRoot false).
	AgentWrites bool
	// Setup makes the canaries the checks use, in a new model.
	Setup func(f *Fake) error
}

// anchor is a harmless first step, so a destructive check can be run
// alone in its group.
func anchor(group string) conformance.Check {
	return conformance.Check{ID: "anchor", Group: group, Class: conformance.Observe,
		Run: func(context.Context, conformance.Env) conformance.CheckResult {
			return conformance.CheckResult{Status: conformance.Pass}
		}}
}

func runOne(cfg Config, ck conformance.Check, m Mutant) (conformance.Status, string, error) {
	f := New("0a1b2c3d")
	f.SetIsolation(cfg.Isolation)
	f.SetRunsAsRoot(!cfg.AgentWrites)
	if err := cfg.Setup(f); err != nil {
		return "", "", err
	}
	f.Break(m)
	r := &conformance.Runner{Env: &Envelope{F: f}, Wipe: true, Restart: f.Restart}
	res, _, err := conformance.RunChecks(context.Background(), r, []conformance.Check{anchor(ck.Group), ck}, f.Env(cfg.Isolation, cfg.Trust))
	if n := f.AgentRuns(); n > 0 {
		return "", "", fmt.Errorf("%d command(s) ran as the agent user in a sandbox that runs as root", n)
	}
	for _, x := range res {
		if x.ID == ck.ID {
			return x.Status, x.Reason, nil
		}
	}
	if err != nil {
		return "", "", err
	}
	return "", "", fmt.Errorf("check %s did not run", ck.ID)
}

// CheckMutants holds the checks to the table, each check alone in a fresh
// model: enforcing, no applicable check may FAIL or ERROR; every PASS check
// must be failed by at least one mutant of the table; and for each mutant
// the checks it is assigned must FAIL, with every other check unchanged
// from enforcing. It fails the test on an assigned id that is no check, a
// mutant that fails nothing, and a mutant that fails a check not assigned
// to it.
func CheckMutants(t testing.TB, checks []conformance.Check, table Table, cfg Config) {
	t.Helper()
	var applicable []conformance.Check
	ids := map[string]bool{}
	for _, c := range checks {
		if c.Applies(cfg.Isolation, cfg.Trust) {
			applicable = append(applicable, c)
			ids[c.ID] = true
		}
	}
	base := map[string]conformance.Status{}
	for _, c := range applicable {
		st, why, err := runOne(cfg, c, Enforcing)
		if err != nil {
			t.Errorf("%s enforcing: %v", c.ID, err)
			continue
		}
		base[c.ID] = st
		if st == conformance.Fail || st == conformance.Error {
			t.Errorf("%s enforcing: %s %s", c.ID, st, why)
		}
	}
	// Every mutant the model acts on, or that a later model registered,
	// must have a row.
	for _, m := range All {
		if _, ok := table[m]; Interpretable(m) && !ok {
			t.Errorf("mutant %q has no row in the table", m)
		}
	}
	for _, m := range Registered() {
		if _, ok := table[m]; !ok {
			t.Errorf("registered mutant %q has no row in the table", m)
		}
	}
	covered := map[string]bool{}
	controlOf := map[string]string{}
	for _, c := range applicable {
		controlOf[c.ID] = c.Control
	}
	controlCovered := map[string]bool{}
	muts := make([]Mutant, 0, len(table))
	for m := range table {
		muts = append(muts, m)
	}
	slices.Sort(muts)
	for _, m := range muts {
		assigned := table[m]
		for _, id := range assigned {
			if !ids[id] {
				t.Errorf("mutant %q is assigned %s, which is not an applicable check", m, id)
			}
			covered[id] = true
			controlCovered[controlOf[id]] = true
		}
		failed := 0
		for _, c := range applicable {
			st, why, err := runOne(cfg, c, m)
			if err != nil {
				t.Errorf("%s under %q: %v", c.ID, m, err)
				continue
			}
			want := base[c.ID]
			if slices.Contains(assigned, c.ID) {
				want = conformance.Fail
			}
			if st == conformance.Fail {
				failed++
			}
			if st != want {
				t.Errorf("%s under %q: %s (%s), want %s", c.ID, m, st, why, want)
			}
		}
		if failed == 0 {
			t.Errorf("mutant %q fails nothing", m)
		}
	}
	for _, c := range applicable {
		if base[c.ID] == conformance.Pass && !covered[c.ID] {
			t.Errorf("check %s (control %q) has no mutant in the table", c.ID, c.Control)
		}
		if base[c.ID] == conformance.Pass && c.Control != "" && !controlCovered[c.Control] {
			t.Errorf("control %q has no mutant of its own that fails its check", c.Control)
		}
	}
}
