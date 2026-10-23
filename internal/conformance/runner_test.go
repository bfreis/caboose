package conformance

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

// fakeEnvelope counts the envelope's calls and records their order.
type fakeEnvelope struct {
	log          []string
	refuseAt     int // the nth Verify (1-based) refuses; 0 never
	verifies     int
	destructives int
	touched      string // a step whose post-check reports a stray change
	last         string
}

func (f *fakeEnvelope) Verify(context.Context) error {
	f.verifies++
	f.log = append(f.log, "verify")
	if f.refuseAt != 0 && f.verifies >= f.refuseAt {
		return refuse("fake", "refused")
	}
	return nil
}

func (f *fakeEnvelope) VerifyDestructive(context.Context) error {
	f.destructives++
	f.log = append(f.log, "verify-destructive")
	return nil
}

func (f *fakeEnvelope) Snapshot() (Snapshot, error) { return Snapshot{}, nil }

func (f *fakeEnvelope) PostCheck(Snapshot, []string) (Snapshot, error) {
	f.log = append(f.log, "post")
	if f.touched != "" && f.touched == f.last {
		return nil, &TouchedError{Paths: []string{"roots/x"}}
	}
	return Snapshot{}, nil
}

func step(f *fakeEnvelope, id, group string, c Class, st Status) Step {
	return Step{ID: id, Group: group, Class: c, Run: func(context.Context) Outcome {
		f.log = append(f.log, "run "+id)
		f.last = id
		return Outcome{Status: st}
	}}
}

func ids(rs []Result) []string {
	var out []string
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%s:%s", r.ID, r.Status))
	}
	return out
}

func TestRunnerOrder(t *testing.T) {
	f := &fakeEnvelope{}
	steps := []Step{
		step(f, "r3", "R", Destructive, Pass),
		step(f, "r1", "R", Observe, Pass),
		step(f, "g1", "G", Write, Pass),
		step(f, "r2", "R", Write, Pass),
		step(f, "r4", "R", Destructive, Pass),
		step(f, "g0", "G", Observe, Pass),
	}
	rs, err := (&Runner{Env: f}).Run(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"r1:PASS", "r2:PASS", "g0:PASS", "g1:PASS", "r3:PASS", "r4:PASS"}
	if !slices.Equal(ids(rs), want) {
		t.Fatalf("order %v", ids(rs))
	}
	wantLog := []string{
		"verify", // start
		"verify", "run r1", "post", "run r2", "post",
		"verify", "run g0", "post", "run g1", "post",
		"verify", "verify-destructive", "run r3", "post", "verify-destructive", "run r4", "post",
	}
	if !slices.Equal(f.log, wantLog) {
		t.Fatalf("log\n%v\nwant\n%v", f.log, wantLog)
	}
}

func TestOrderRefuses(t *testing.T) {
	f := &fakeEnvelope{}
	for name, steps := range map[string][]Step{
		"destructive first": {step(f, "d", "R", Destructive, Pass)},
		"a wipe that is not destructive": {
			step(f, "o", "R", Observe, Pass), func() Step { s := step(f, "w", "R", Write, Pass); s.Wipe = true; return s }(),
		},
		"an id twice":    {step(f, "o", "R", Observe, Pass), step(f, "o", "G", Observe, Pass)},
		"nothing to run": {{ID: "x", Group: "R"}},
	} {
		if _, err := (&Runner{Env: f}).Run(context.Background(), steps); err == nil {
			t.Fatalf("%s: ordered", name)
		}
	}
	if len(f.log) != 0 {
		t.Fatalf("ran %v", f.log)
	}
}

// Every destructive step runs after every step that is not, whatever its
// group: groups keep the order they first appear in, both among the
// observe and write steps and among the destructive ones.
func TestOrderDestructiveLastAcrossGroups(t *testing.T) {
	f := &fakeEnvelope{}
	steps := []Step{
		step(f, "a-o", "A", Observe, Pass),
		step(f, "a-d2", "A", Destructive, Pass),
		step(f, "b-w", "B", Write, Pass),
		step(f, "a-w", "A", Write, Pass),
		step(f, "a-d1", "A", Destructive, Pass),
		step(f, "c-o", "C", Observe, Pass),
		step(f, "c-d", "C", Destructive, Pass),
		step(f, "b-d", "B", Destructive, Pass),
		step(f, "b-o", "B", Observe, Pass),
	}
	got, err := Order(steps)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, s := range got {
		order = append(order, s.ID)
	}
	want := []string{"a-o", "a-w", "b-o", "b-w", "c-o", "a-d2", "a-d1", "b-d", "c-d"}
	if !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	// A group of destructive steps alone is still refused, wherever it is.
	if _, err := Order(append(steps, step(f, "z-d", "Z", Destructive, Pass))); err == nil {
		t.Fatal("a group of destructive steps alone was ordered")
	}
}

// An ERROR in a later group's observe step skips an earlier group's
// destructive step, which now runs after it.
func TestRunnerLaterErrorStopsEarlierDestructive(t *testing.T) {
	f := &fakeEnvelope{}
	steps := []Step{
		step(f, "a-o", "A", Observe, Pass),
		step(f, "a-d", "A", Destructive, Pass),
		step(f, "b-o", "B", Observe, Error),
		step(f, "b-d", "B", Destructive, Pass),
	}
	rs, err := (&Runner{Env: f}).Run(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a-o:PASS", "b-o:ERROR", "a-d:SKIP", "b-d:SKIP"}; !slices.Equal(ids(rs), want) {
		t.Fatalf("%v", ids(rs))
	}
	if f.destructives != 0 || slices.Contains(f.log, "run a-d") || slices.Contains(f.log, "run b-d") {
		t.Fatalf("a destructive ran: %v", f.log)
	}
}

func TestRunnerErrorStopsDestructive(t *testing.T) {
	f := &fakeEnvelope{}
	steps := []Step{
		step(f, "o", "R", Observe, Error),
		step(f, "d", "R", Destructive, Pass),
		step(f, "g", "G", Observe, Pass),
		step(f, "gd", "G", Destructive, Pass),
	}
	rs, err := (&Runner{Env: f}).Run(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"o:ERROR", "g:PASS", "d:SKIP", "gd:SKIP"}; !slices.Equal(ids(rs), want) {
		t.Fatalf("%v", ids(rs))
	}
	if f.destructives != 0 || slices.Contains(f.log, "run d") {
		t.Fatalf("a destructive ran: %v", f.log)
	}
}

func TestRunnerDestructiveFailEnds(t *testing.T) {
	f := &fakeEnvelope{}
	steps := []Step{
		step(f, "o", "R", Observe, Pass),
		step(f, "d1", "R", Destructive, Fail),
		step(f, "d2", "R", Destructive, Pass),
		step(f, "g", "G", Observe, Pass),
		step(f, "gd", "G", Destructive, Pass),
	}
	rs, err := (&Runner{Env: f}).Run(context.Background(), steps)
	if err == nil {
		t.Fatal("the run went on")
	}
	if want := []string{"o:PASS", "g:PASS", "d1:FAIL", "d2:SKIP", "gd:SKIP"}; !slices.Equal(ids(rs), want) {
		t.Fatalf("%v", ids(rs))
	}
}

func TestRunnerWipe(t *testing.T) {
	for _, wipe := range []bool{false, true} {
		f := &fakeEnvelope{}
		w := step(f, "wipe", "R", Destructive, Pass)
		w.Wipe = true
		steps := []Step{step(f, "o", "R", Observe, Pass), step(f, "d", "R", Destructive, Pass), w}
		rs, err := (&Runner{Env: f, Wipe: wipe}).Run(context.Background(), steps)
		if err != nil {
			t.Fatal(err)
		}
		want := "wipe:SKIP"
		if wipe {
			want = "wipe:PASS"
		}
		if got := ids(rs); got[2] != want || got[1] != "d:PASS" {
			t.Fatalf("wipe %t: %v", wipe, got)
		}
		if wipe && f.destructives != 2 || !wipe && f.destructives != 1 {
			t.Fatalf("wipe %t: %d destructive verifies", wipe, f.destructives)
		}
	}
}

// The wipe runs after every other destructive step, whatever its group
// and wherever it was given.
func TestOrderWipeLast(t *testing.T) {
	f := &fakeEnvelope{}
	w := step(f, "a-wipe", "A", Destructive, Pass)
	w.Wipe = true
	steps := []Step{
		step(f, "a-o", "A", Observe, Pass), w, step(f, "a-d", "A", Destructive, Pass),
		step(f, "b-o", "B", Observe, Pass), step(f, "b-d", "B", Destructive, Pass),
	}
	got, err := Order(steps)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, s := range got {
		order = append(order, s.ID)
	}
	if want := []string{"a-o", "b-o", "a-d", "b-d", "a-wipe"}; !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
}

// An ERROR in any earlier step, destructive or not, skips the wipe; a
// destructive ERROR still lets the other destructive steps run.
func TestRunnerErrorSkipsWipe(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   Class
		want []string
	}{
		{"observe", Observe, []string{"o:ERROR", "d:SKIP", "wipe:SKIP"}},
		{"destructive", Destructive, []string{"o:PASS", "d:ERROR", "d2:PASS", "wipe:SKIP"}},
	} {
		f := &fakeEnvelope{}
		w := step(f, "wipe", "R", Destructive, Pass)
		w.Wipe, w.Fresh = true, true
		steps := []Step{step(f, "o", "R", Observe, Pass), step(f, "d", "R", Destructive, Pass), w}
		if tc.at == Observe {
			steps[0] = step(f, "o", "R", Observe, Error)
		} else {
			steps[1] = step(f, "d", "R", Destructive, Error)
			steps = append(steps, step(f, "d2", "R", Destructive, Pass))
		}
		restarts := 0
		rs, err := (&Runner{Env: f, Wipe: true, Restart: func(context.Context) error { restarts++; return nil }}).Run(context.Background(), steps)
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(rs); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: %v, want %v", tc.name, got, tc.want)
		}
		if slices.Contains(f.log, "run wipe") || restarts != 0 {
			t.Fatalf("%s: the wipe ran or restarted: %v, %d", tc.name, f.log, restarts)
		}
		if r := rs[len(rs)-1]; r.Reason != "an earlier check was an ERROR" {
			t.Fatalf("%s: %q", tc.name, r.Reason)
		}
	}
}

// A Fresh wipe restarts the sandbox, then verifies, then runs.
func TestRunnerFreshWipeRestarts(t *testing.T) {
	f := &fakeEnvelope{}
	w := step(f, "wipe", "R", Destructive, Pass)
	w.Wipe, w.Fresh = true, true
	restart := func(context.Context) error { f.log = append(f.log, "restart"); return nil }
	rs, err := (&Runner{Env: f, Wipe: true, Restart: restart}).Run(context.Background(), []Step{step(f, "o", "R", Observe, Pass), w})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(rs); !slices.Equal(got, []string{"o:PASS", "wipe:PASS"}) {
		t.Fatalf("%v", got)
	}
	i := slices.Index(f.log, "restart")
	if i < 0 || f.log[i+1] != "verify-destructive" || f.log[i+2] != "run wipe" {
		t.Fatalf("log %v", f.log)
	}
}

func TestRunnerRefusals(t *testing.T) {
	// Refused at start: nothing runs.
	f := &fakeEnvelope{refuseAt: 1}
	steps := []Step{step(f, "o", "R", Observe, Pass), step(f, "g", "G", Observe, Pass)}
	if rs, err := (&Runner{Env: f}).Run(context.Background(), steps); err == nil || len(rs) != 0 {
		t.Fatalf("%v %v", rs, err)
	}
	// Refused before the second group: it does not run.
	f = &fakeEnvelope{refuseAt: 3}
	steps = []Step{step(f, "o", "R", Observe, Pass), step(f, "g", "G", Observe, Pass)}
	rs, err := (&Runner{Env: f}).Run(context.Background(), steps)
	if err == nil || slices.Contains(f.log, "run g") || ids(rs)[1] != "g:SKIP" {
		t.Fatalf("%v %v %v", ids(rs), err, f.log)
	}
	// A stray change ends the run with an ERROR.
	f = &fakeEnvelope{touched: "o"}
	steps = []Step{step(f, "o", "R", Observe, Pass), step(f, "w", "R", Write, Pass)}
	rs, err = (&Runner{Env: f}).Run(context.Background(), steps)
	var te *TouchedError
	if !errors.As(err, &te) || !slices.Equal(ids(rs), []string{"o:ERROR", "w:SKIP"}) {
		t.Fatalf("%v %v", ids(rs), err)
	}
	// The check's own FAIL is kept in the ERROR's reason.
	f = &fakeEnvelope{touched: "o"}
	failing := step(f, "o", "R", Observe, Fail)
	failing.Run = func(context.Context) Outcome { f.last = "o"; return Outcome{Fail, "the original reason"} }
	rs, _ = (&Runner{Env: f}).Run(context.Background(), []Step{failing})
	if rs[0].Status != Error || !strings.Contains(rs[0].Reason, "the original reason") || !strings.Contains(rs[0].Reason, "FAIL") {
		t.Fatalf("%+v", rs[0])
	}
	// A fresh step without a way to restart is an ERROR, and stops the
	// destructive ones.
	f = &fakeEnvelope{}
	fresh := step(f, "f", "R", Write, Pass)
	fresh.Fresh = true
	steps = []Step{step(f, "o", "R", Observe, Pass), fresh, step(f, "d", "R", Destructive, Pass)}
	rs, err = (&Runner{Env: f}).Run(context.Background(), steps)
	if err != nil || !slices.Equal(ids(rs), []string{"o:PASS", "f:ERROR", "d:SKIP"}) {
		t.Fatalf("%v %v", ids(rs), err)
	}
	// A check that returns no status is an ERROR, never a PASS.
	f = &fakeEnvelope{}
	steps = []Step{step(f, "o", "R", Observe, "")}
	if rs, _ := (&Runner{Env: f}).Run(context.Background(), steps); rs[0].Status != Error {
		t.Fatalf("%v", ids(rs))
	}
	// An interrupt skips what is left.
	ctx, cancel := context.WithCancel(context.Background())
	f = &fakeEnvelope{}
	o := step(f, "o", "R", Observe, Pass)
	run := o.Run
	o.Run = func(c context.Context) Outcome { cancel(); return run(c) }
	steps = []Step{o, step(f, "w", "R", Write, Pass)}
	if rs, err := (&Runner{Env: f}).Run(ctx, steps); err == nil || !slices.Equal(ids(rs), []string{"o:PASS", "w:SKIP"}) {
		t.Fatalf("%v %v", ids(rs), err)
	}
}

// With the real guard: Verify runs before each destructive step, and the
// steps touch only their canaries whatever order they come in and
// whichever of them "fail" by changing what they were handed.
func TestRunnerWithGuard(t *testing.T) {
	ctx := context.Background()
	for seed := uint64(1); seed <= 20; seed++ {
		r := newRig(t, config.KindContainer)
		if _, err := r.g.Pin(ctx); err != nil {
			t.Fatal(err)
		}
		rng := rand.New(rand.NewPCG(seed, seed))
		part := "roots/" + r.g.RootNames()[1] + "/" + WritablePart
		var steps []Step
		nd := 0
		for i := 0; i < 8; i++ {
			c, err := r.g.MakeCanary(part, fmt.Sprintf("c%d", i), File)
			if err != nil {
				t.Fatal(err)
			}
			class := Class(rng.IntN(3))
			if i == 0 {
				class = Observe
			}
			if class == Destructive {
				nd++
			}
			broken := rng.IntN(2) == 0
			steps = append(steps, Step{ID: c.Role, Group: "R", Class: class, Canaries: []string{c.Rel}, Run: func(context.Context) Outcome {
				if broken {
					r.write(c.Rel, "changed")
					return Outcome{Status: Fail}
				}
				return Outcome{Status: Pass}
			}})
		}
		rng.Shuffle(len(steps)-1, func(i, j int) { steps[i+1], steps[j+1] = steps[j+1], steps[i+1] })
		before := r.g.Verifies()
		rs, err := (&Runner{Env: r.g}).Run(ctx, steps)
		if err != nil && (len(rs) == 0 || !slices.ContainsFunc(rs, func(x Result) bool { return x.Class == Destructive && x.Status == Fail })) {
			t.Fatalf("seed %d: %v", seed, err)
		}
		ran := 0
		for _, res := range rs {
			if res.Class == Destructive && res.Status != Skip {
				ran++
			}
		}
		// Start, the group, and one per destructive step that ran.
		if got := r.g.Verifies() - before; got != 2+ran {
			t.Fatalf("seed %d: %d verifies for %d destructive steps", seed, got, ran)
		}
	}
	// A stray write by a step is caught and ends the run.
	r := newRig(t, config.KindContainer)
	c, err := r.g.MakeCanary("roots/"+r.g.RootNames()[1]+"/"+WritablePart, "c", File)
	if err != nil {
		t.Fatal(err)
	}
	steps := []Step{
		{ID: "o", Group: "R", Class: Observe, Canaries: []string{c.Rel}, Run: func(context.Context) Outcome {
			r.write("roots/"+r.g.RootNames()[0]+"/stray", "x")
			return Outcome{Status: Pass}
		}},
		{ID: "w", Group: "R", Class: Write, Run: func(context.Context) Outcome { return Outcome{Status: Pass} }},
	}
	rs, err := (&Runner{Env: r.g}).Run(ctx, steps)
	var te *TouchedError
	if !errors.As(err, &te) || rs[0].Status != Error || rs[1].Status != Skip {
		t.Fatalf("%v %v", ids(rs), err)
	}
}
