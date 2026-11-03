package checks_test

import (
	"strings"
	"testing"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/checks"
	"github.com/bfreis/caboose/internal/conformance/fake"
)

func n1Of(t *testing.T) c.Check {
	t.Helper()
	cks := checks.NoRootsChecks()
	if len(cks) != 1 || cks[0].ID != "N1" {
		t.Fatalf("the no-roots checks are %v", cks)
	}
	if cks[0].Destructive() {
		t.Fatal("N1 is destructive")
	}
	return cks[0]
}

// noRootsWorld is the suite's layout on the host, under a sandbox made with
// no roots, as a run with no roots lays it out.
func noRootsWorld(t *testing.T, iso string) *fake.Fake {
	t.Helper()
	f := world(t, iso)
	f.SetNoRoots()
	return f
}

func runN1(t *testing.T, f *fake.Fake, iso string) (c.Status, string) {
	t.Helper()
	res, _ := runWith(f, f, iso, n1Of(t))
	return status(res, "N1")
}

func TestN1PassesWithNoRoots(t *testing.T) {
	for _, iso := range []string{isoContainer, isoGvisor, isoVM} {
		f := noRootsWorld(t, iso)
		made := len(f.Mutations())
		if st, why := runN1(t, f, iso); st != c.Pass {
			t.Errorf("%s: %s (%s), want PASS", iso, st, why)
		}
		for _, m := range f.Mutations()[made:] {
			t.Errorf("%s: N1 changed %s %s", iso, m.Op, m.Rel)
		}
	}
}

// A sandbox with the suite's usual two roots is what N1 exists to refuse.
func TestN1FailsWithRoots(t *testing.T) {
	f := world(t, isoContainer)
	if st, why := runN1(t, f, isoContainer); st != c.Fail {
		t.Errorf("%s (%s), want FAIL", st, why)
	}
}

// Each mutant breaks one thing, and what it says tells which of N1's looks
// caught it: none of them is vacuous.
func TestN1Mutants(t *testing.T) {
	want := map[fake.Mutant]string{
		fake.NoRootsRootStaysMounted:     "is mounted",
		fake.NoRootsDefaultRoot:          "/work/dev is mounted",
		fake.NoRootsLabelLists:           "the roots label lists",
		fake.NoRootsLabelNull:            "the roots label is missing or is no list",
		fake.NoRootsWritableLabel:        "the writable label names a root",
		fake.NoRootsHiddenMount:          "is in the sandbox's mount table",
		fake.NoRootsRootMountedElsewhere: "is mounted from a root's directory",
		fake.NoRootsCanaryReadable:       "can be read in the sandbox",
		fake.NoRootsWorkHolds:            `/work holds "something"`,
	}
	if len(want) != len(fake.NoRootsMutants) {
		t.Fatalf("%d mutants tried of %d", len(want), len(fake.NoRootsMutants))
	}
	for _, iso := range []string{isoContainer, isoGvisor, isoVM} {
		for _, m := range fake.NoRootsMutants {
			f := noRootsWorld(t, iso)
			f.Break(m)
			st, why := runN1(t, f, iso)
			if st != c.Fail || !strings.Contains(why, want[m]) {
				t.Errorf("%s under %q: %s (%s), want FAIL saying %q", iso, m, st, why, want[m])
			}
		}
	}
}

// What leaves N1 unable to tell is an ERROR, never a PASS: a read that
// fails for every path looks just like nothing being there.
func TestN1NoVacuousPass(t *testing.T) {
	for name, set := range map[string]func(*fake.Fake){
		"sandbox down":        func(f *fake.Fake) { f.SetDown(true) },
		"shell missing":       func(f *fake.Fake) { f.SetShellMissing(true) },
		"cat refused":         func(f *fake.Fake) { f.SetCatDenied() },
		"cat missing":         func(f *fake.Fake) { f.SetCatMissing() },
		"canary not on host":  func(f *fake.Fake) { f.HideCanary("um-ro") },
		"managed not on host": func(f *fake.Fake) { f.HideCanary("managed") },
		"no user label":       func(f *fake.Fake) { f.SetUserLabel("", true) },
	} {
		f := noRootsWorld(t, isoContainer)
		set(f)
		if st, why := runN1(t, f, isoContainer); st != c.Error {
			t.Errorf("%s: %s (%s), want ERROR", name, st, why)
		}
	}
}

// N1 is in the suite's list, for the matrix, and in no ordinary run: it
// cannot pass in a sandbox with roots.
func TestN1IsInEveryAndNotInTheRuns(t *testing.T) {
	found := false
	for _, ck := range checks.Every() {
		found = found || ck.ID == "N1"
	}
	if !found {
		t.Error("N1 is not in Every")
	}
	for _, ck := range append(checks.All(), checks.ForRun(isoVM, c.TrustHostile)...) {
		if ck.ID == "N1" {
			t.Error("N1 runs in an ordinary run")
		}
	}
}
