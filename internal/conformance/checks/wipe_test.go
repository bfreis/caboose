package checks_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	c "github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/conformance/checks"
	"github.com/bfreis/caboose/internal/conformance/fake"
)

func TestR14IsTheWipe(t *testing.T) {
	ck := byID(t, "R14")
	if !ck.Wipe || ck.Class != c.Destructive || !ck.Fresh {
		t.Fatalf("R14 is %+v", ck)
	}
	order, err := c.RunOrder(wipeFirst())
	if err != nil {
		t.Fatal(err)
	}
	if last := order[len(order)-1]; last.ID != "R14" {
		t.Fatalf("the last check is %s", last.ID)
	}
}

// wipeFirst are every check of the suite, the wipe given first, so that
// it is the runner that puts it last.
func wipeFirst() []c.Check {
	all := checks.Every()
	i := slices.IndexFunc(all, func(ck c.Check) bool { return ck.ID == "R14" })
	return append([]c.Check{all[i]}, append(all[:i:i], all[i+1:]...)...)
}

// On the honest model the wipe removes the writable part's canaries and
// nothing else, after a restart, as root, behind the destructive verify.
func TestR14PassesOnTheModel(t *testing.T) {
	for _, w := range sandboxWorlds {
		f := world(t, w.iso)
		f.SetRunsAsRoot(!w.agentWrites)
		res, col := runWith(f, f, w.iso, byID(t, "R14"))
		st, why := status(res, "R14")
		if st != c.Pass {
			t.Fatalf("%s: %s %s", w.iso, st, why)
		}
		for _, want := range []string{"canaries gone, as expected", "ran as root", "destroyed by design"} {
			if !strings.Contains(why, want) {
				t.Errorf("%s: the reason %q lacks %q", w.iso, why, want)
			}
		}
		if !slices.Contains(col.Lines("R14"), "ran as: root") {
			t.Errorf("%s: lines %v", w.iso, col.Lines("R14"))
		}
		if f.Restarts() != 1 {
			t.Errorf("%s: %d restarts", w.iso, f.Restarts())
		}
		ev := f.Events()
		i := slices.Index(ev, "restart")
		if i < 0 || !slices.Equal(ev[i+1:i+3], []string{"verify-destructive", "exec-destructive"}) {
			t.Errorf("%s: events %v", w.iso, ev)
		}
		if err := fake.CheckOrder(ev); err != nil {
			t.Errorf("%s: %v", w.iso, err)
		}
	}
}

// A read-only root that is in fact writable is a FAIL naming a canary of
// it; so are a writable part's parent and a managed directory that are.
func TestR14FailsItsMutants(t *testing.T) {
	for m, zone := range map[fake.Mutant]string{
		fake.RootsWritable:      "(read-only root)",
		fake.ParentWritable:     "(writable root, outside its part)",
		fake.ManagedWritable:    "(managed directory)",
		fake.WipeReachesOutside: "(outside every mount)",
	} {
		f := world(t, isoContainer)
		f.Break(m)
		res, _ := runWith(f, f, isoContainer, byID(t, "R14"))
		if st, why := status(res, "R14"); st != c.Fail || !strings.Contains(why, zone) {
			t.Errorf("%q: %s %s", m, st, why)
		}
	}
}

func TestR14SkippedWithoutTheWipe(t *testing.T) {
	f := world(t, isoVM)
	env := c.NewEnv(f, f, f, f, isoVM, "fallible")
	r := &c.Runner{Env: &fake.Envelope{F: f}, Wipe: false, Restart: f.Restart}
	res, _, err := c.RunChecks(context.Background(), r, []c.Check{anchor("files"), byID(t, "R14")}, env)
	if err != nil {
		t.Fatal(err)
	}
	if st, why := status(res, "R14"); st != c.Skip || why != "the wipe is turned off" {
		t.Fatalf("%s %s", st, why)
	}
	if f.Restarts() != 0 || slices.Contains(f.Events(), "exec-destructive") {
		t.Fatalf("the sandbox was used: %v", f.Events())
	}
}

// An ERROR in any check before it, destructive or not, skips the wipe.
func TestR14SkippedAfterAnError(t *testing.T) {
	for _, class := range []c.Class{c.Observe, c.Destructive} {
		f := world(t, isoVM)
		bad := c.Check{ID: "X1", Group: "files", Class: class,
			Run: func(context.Context, c.Env) c.CheckResult { return c.CheckResult{Status: c.Error, Reason: "broken"} }}
		res, _ := runWith(f, f, isoVM, byID(t, "R14"), bad)
		if st, why := status(res, "R14"); st != c.Skip || why != "an earlier check was an ERROR" {
			t.Errorf("after a %s ERROR: %s %s", class, st, why)
		}
		if f.Restarts() != 0 || slices.Contains(f.Events(), "exec-destructive") {
			t.Errorf("after a %s ERROR the sandbox was used: %v", class, f.Events())
		}
	}
}

// slowSandbox is the model, its wipe running out of time: the wipe is run
// (or not), then the exec reports the deadline.
type slowSandbox struct {
	*fake.Fake
	run      bool
	deadline time.Duration
}

func (s *slowSandbox) Exec(ctx context.Context, user string, argv []string, stdin []byte) (int, string, string, error) {
	body, _ := c.Unwrap(argv)
	if !strings.HasPrefix(body, "rm -rf ") {
		return s.Fake.Exec(ctx, user, argv, stdin)
	}
	if d, ok := ctx.Deadline(); ok {
		s.deadline = time.Until(d)
	}
	if s.run {
		s.Fake.Exec(ctx, user, argv, stdin)
	}
	return -1, "", "", context.DeadlineExceeded
}

// The wipe is bounded: past its deadline it is an ERROR, and the host's
// side is still compared, so a canary it destroyed is a FAIL.
func TestR14Timeout(t *testing.T) {
	for _, tc := range []struct {
		mut  fake.Mutant
		run  bool
		want c.Status
	}{
		{fake.Enforcing, false, c.Error},
		{fake.Enforcing, true, c.Error},
		{fake.RootsWritable, true, c.Fail},
	} {
		f := world(t, isoContainer)
		f.Break(tc.mut)
		sb := &slowSandbox{Fake: f, run: tc.run}
		res, _ := runWith(f, sb, isoContainer, byID(t, "R14"))
		st, why := status(res, "R14")
		if st != tc.want {
			t.Errorf("%q run=%v: %s %s", tc.mut, tc.run, st, why)
		}
		if tc.want == c.Error && (!strings.Contains(why, "did not end within 2m0s") || !strings.Contains(why, "may still be running in the sandbox")) {
			t.Errorf("%q run=%v: %q", tc.mut, tc.run, why)
		}
		if sb.deadline <= 0 || sb.deadline > 2*time.Minute {
			t.Errorf("the wipe's deadline was %s away", sb.deadline)
		}
	}
}

// The wipe's command is the run's two roots and its managed canaries,
// literal, and the only command of the run with -rf.
func TestR14Command(t *testing.T) {
	f := world(t, isoVM)
	rec := &recorder{Fake: f}
	runWith(f, rec, isoVM, byID(t, "R14"))
	var wipes []string
	for _, b := range rec.cmds {
		if strings.Contains(b, "rm ") {
			wipes = append(wipes, b)
		}
	}
	if len(wipes) != 1 {
		t.Fatalf("commands %q", wipes)
	}
	toks := strings.Fields(wipes[0])
	if len(toks) < 4 || toks[0] != "rm" || toks[1] != "-rf" || toks[2] != "/work/conf-0a1b2c3d-ro" || toks[3] != "/work/conf-0a1b2c3d-dev" {
		t.Fatalf("%q", wipes[0])
	}
	for _, tok := range toks[4:] {
		if !strings.HasPrefix(tok, "/etc/claude-code/conf-0a1b2c3d-") || strings.Count(tok, "/") != 3 {
			t.Errorf("the wipe names %q", tok)
		}
	}
}

// A canary outside the part that changed after the last post-check (here
// during the restart) is held to the host's record, not to what R14 finds
// when it starts: a FAIL, and the wipe is not run on top of it.
func TestR14ChangeDuringRestartFails(t *testing.T) {
	f := world(t, isoContainer)
	ro, err := f.Canary("ro-file")
	if err != nil {
		t.Fatal(err)
	}
	env := c.NewEnv(f, f, f, f, isoContainer, "fallible")
	restart := func(ctx context.Context) error {
		if err := f.Restart(ctx); err != nil {
			return err
		}
		f.Tamper(ro.Rel)
		return nil
	}
	r := &c.Runner{Env: &fake.Envelope{F: f}, Wipe: true, Restart: restart}
	res, _, _ := c.RunChecks(context.Background(), r, []c.Check{anchor("files"), byID(t, "R14")}, env)
	st, why := status(res, "R14")
	if st != c.Fail || !strings.Contains(why, "ro-file") || !strings.Contains(why, "before the wipe") {
		t.Fatalf("%s %s", st, why)
	}
	if slices.Contains(f.Events(), "exec-destructive") {
		t.Fatalf("the wipe ran: %v", f.Events())
	}
}

// runFaulted runs R14 after the given checks, fault applied to the model
// during R14's restart.
func runFaulted(t *testing.T, f *fake.Fake, fault func(), before ...c.Check) []c.Result {
	t.Helper()
	env := c.NewEnv(f, f, f, f, isoContainer, "fallible")
	restart := func(ctx context.Context) error {
		if err := f.Restart(ctx); err != nil {
			return err
		}
		fault()
		return nil
	}
	r := &c.Runner{Env: &fake.Envelope{F: f}, Wipe: true, Restart: restart}
	res, _, _ := c.RunChecks(context.Background(), r, append(append([]c.Check{anchor("files")}, before...), byID(t, "R14")), env)
	return res
}

func wantNotRun(t *testing.T, f *fake.Fake, res []c.Result, role string) {
	t.Helper()
	st, why := status(res, "R14")
	if st != c.Fail || !strings.Contains(why, role) || !strings.Contains(why, "before the wipe") {
		t.Fatalf("%s %s", st, why)
	}
	if slices.Contains(f.Events(), "exec-destructive") {
		t.Fatalf("the wipe ran: %v", f.Events())
	}
}

// A read-only root's canary no check was handed, gone during the restart:
// a FAIL, and the wipe not run.
func TestR14RemovalDuringRestartFails(t *testing.T) {
	f := world(t, isoContainer)
	ro, err := f.Canary("ro-file")
	if err != nil {
		t.Fatal(err)
	}
	res := runFaulted(t, f, func() { f.Vanish(ro.Rel) })
	wantNotRun(t, f, res, "ro-file")
}

// A canary its check was handed and removed, back during the restart: the
// record says removed, so a FAIL, and the wipe not run.
func TestR14RemovedCanaryBackFails(t *testing.T) {
	f := world(t, isoContainer)
	ro, err := f.Canary("ro-file")
	if err != nil {
		t.Fatal(err)
	}
	remover := c.Check{ID: "X1", Group: "files", Class: c.Destructive, Roles: []string{"ro-file"},
		Run: func(context.Context, c.Env) c.CheckResult {
			f.Vanish(ro.Rel)
			return c.CheckResult{Status: c.Pass}
		}}
	res := runFaulted(t, f, func() { f.Revive(ro.Rel) }, remover)
	if st, why := status(res, "X1"); st != c.Pass {
		t.Fatalf("X1: %s %s", st, why)
	}
	wantNotRun(t, f, res, "ro-file")
}
