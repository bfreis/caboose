package conformance_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/fake"
)

// A table without a mutant's row, or without a mutant for a control, is
// refused.
func TestCheckMutantsNeedsEveryRowAndControl(t *testing.T) {
	cfg := fake.Config{Isolation: isoContainer, Trust: "fallible", Setup: setup}
	tb := fake.Table{}
	for k, v := range table {
		tb[k] = v
	}
	delete(tb, fake.SymlinkFollowed)
	r := &recorder{TB: t}
	fake.CheckMutants(r, allChecks(), tb, cfg)
	if !containsAny(r.errs, `mutant "symlink followed" has no row`) {
		t.Errorf("a missing row not caught: %v", r.errs)
	}
	tb = fake.Table{}
	for k, v := range table {
		tb[k] = v
	}
	tb[fake.UnmountWritesThrough] = nil
	r = &recorder{TB: t}
	fake.CheckMutants(r, allChecks(), tb, cfg)
	if !containsAny(r.errs, `control "the part cannot be unmounted" has no mutant`) {
		t.Errorf("a control without a mutant not caught: %v", r.errs)
	}
}

func TestReportKeepsEveryStatus(t *testing.T) {
	checks := []c.Check{{ID: "A", Title: "ta"}, {ID: "B", Title: "tb"}, {ID: "C", Title: "tc"}, {ID: "D", Title: "td"}, {ID: "E", Title: "te"}, {ID: "P", Title: "tp"}}
	res := []c.Result{
		{ID: "P", Outcome: c.Outcome{Status: c.Pass}},
		{ID: "A", Outcome: c.Outcome{Status: c.Fail, Reason: "why a"}},
		{ID: "B", Outcome: c.Outcome{Status: c.Error, Reason: "why b"}},
		{ID: "C", Outcome: c.Outcome{Status: c.Gap, Reason: "why c"}},
		{ID: "D", Outcome: c.Outcome{Status: c.Skip, Reason: "why d"}},
		{ID: "E", Outcome: c.Outcome{Status: c.Info, Reason: "why e"}},
	}
	rep := c.BuildReport(header(), checks, res, nil)
	if len(rep.Entries) != len(res) {
		t.Fatalf("%d entries for %d results", len(rep.Entries), len(res))
	}
	for _, s := range []c.Status{c.Pass, c.Fail, c.Error, c.Gap, c.Skip, c.Info} {
		if rep.Counts()[s] != 1 {
			t.Errorf("%s counted %d", s, rep.Counts()[s])
		}
	}
	out, err := rep.Format("", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PASS P tp", "FAIL A ta", "reason: why a", "ERROR B tb", "reason: why b", "GAP C tc",
		"SKIP D td", "INFO E te", "summary: 6 checks: 1 PASS, 1 FAIL, 1 ERROR, 1 GAP, 1 SKIP, 1 INFO"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// failingSnapshot is a host whose read-back of a canary fails from the nth
// Snapshot on.
type failingSnapshot struct {
	*fake.Fake
	n, from int
}

func (h *failingSnapshot) Snapshot(cn c.Canary) (map[string]c.Identity, error) {
	h.n++
	if h.n >= h.from {
		return nil, errors.New("host read failed")
	}
	return h.Fake.Snapshot(cn)
}

func TestRefusalHostReadBackErrorIsError(t *testing.T) {
	// Snapshots: allowed before, target before, allowed after, target after.
	for from, name := range map[int]string{4: "the protected canary's read-back", 3: "the allowed canary's read-back"} {
		f, env := newWorld(t, isoContainer)
		h := &failingSnapshot{Fake: f, from: from}
		e := c.NewEnv(f, h, f, isoContainer, "fallible")
		res, _, err := c.RunChecks(context.Background(), &c.Runner{Env: env, Wipe: true}, refusals()[:1], e)
		if err != nil {
			t.Fatal(err)
		}
		if res[0].Status != c.Error {
			t.Errorf("%s: %s %s, want ERROR", name, res[0].Status, res[0].Reason)
		}
	}
}

func TestRunCmdUnsealed(t *testing.T) {
	f, _ := newWorld(t, isoContainer)
	e := f.Env(isoContainer, "fallible")
	cn, err := e.Canary("wr-file")
	if err == nil {
		t.Fatal("an unsealed Env gave a canary")
	}
	_ = cn
	// Make the command from the host's own canary, so only the seal stops it.
	hc, err := f.Canary("wr-file")
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.SandboxPath(hc)
	if err != nil {
		t.Fatal(err)
	}
	cmd, _ := c.Append(p)
	if _, err := e.RunCmd(context.Background(), "agent", cmd); !errors.Is(err, c.ErrNoEnvelope) {
		t.Errorf("RunCmd unsealed: %v", err)
	}
	if ev := f.Events(); len(ev) != 0 {
		t.Errorf("the sandbox ran something: %v", ev)
	}
}

func TestPositiveControlNeedsSandboxAndHostToAgree(t *testing.T) {
	f1 := refusals()[0]
	// The sandbox says refused, the host sees the change.
	r := runRefusal(t, fake.Enforcing, f1, func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
		if n == 1 {
			real()
			return say("sh: 1: cannot create x: Read-only file system", 2)
		}
		return real()
	})
	if r.Status != c.Error || !strings.Contains(r.Reason, "positive control failed") {
		t.Errorf("rc!=0 with a host change: %s %q", r.Status, r.Reason)
	}
	// The sandbox says ok, the host sees nothing.
	r = runRefusal(t, fake.Enforcing, f1, func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
		if n == 1 {
			return say("", 0)
		}
		return real()
	})
	if r.Status != c.Error || !strings.Contains(r.Reason, "did not change") {
		t.Errorf("rc=0 without a host change: %s %q", r.Status, r.Reason)
	}
}

func TestWrapperMessageIsStderrNotStdout(t *testing.T) {
	f1 := refusals()[0]
	// stdout carries a refusal-looking line, stderr is empty: no message.
	r := runRefusal(t, fake.Enforcing, f1, func(n int, a []string, real func() (int, string, string, error)) (int, string, string, error) {
		if n == 2 {
			return 0, "cannot create x: Read-only file system\n\nrc=1\n", "", nil
		}
		return real()
	})
	if r.Status == c.Pass {
		t.Errorf("stdout was taken for the message: %s %q", r.Status, r.Reason)
	}
}
