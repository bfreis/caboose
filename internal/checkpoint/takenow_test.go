package checkpoint

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// A checkpoint asked for now is taken with its reason, and named by its
// ID; a path the view does not list is not.
func TestTakeNow(t *testing.T) {
	w := newSchedWorld(t, nil)
	p := w.env.paths[0]
	got := w.s.TakeNow(t.Context(), []string{p, "/work/other"}, ReasonAgent)
	if len(got) != 2 || !got[0].Taken || got[0].Path != p || got[1].Taken || !strings.Contains(got[1].Reason, "not one of") {
		t.Fatalf("TakeNow = %+v", got)
	}
	if calls := w.take.calls(); !slices.Equal(calls, []string{ReasonAgent}) {
		t.Fatalf("takes %v", calls)
	}
	es, err := w.env.store(t).List(w.key())
	if err != nil || len(es) != 1 || es[0].ID != got[0].ID || es[0].Meta == nil || es[0].Meta.Reason != ReasonAgent {
		t.Fatalf("store holds %+v (%v), TakeNow said %s", es, err, got[0].ID)
	}
	// The schedule counts it: no start checkpoint right after it.
	w.tick(t)
	if calls := w.take.calls(); len(calls) != 1 {
		t.Fatalf("the tick after took %v", calls)
	}
}

// It never runs beside another checkpoint: with the gate taken, it waits
// until its context ends and takes nothing; with the store's lock held it
// takes nothing.
func TestTakeNowWaitsForTheGate(t *testing.T) {
	gate := NewGate()
	w := newSchedWorld(t, gate)
	if !gate.TryAcquire() {
		t.Fatal("gate taken")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got := w.s.TakeNow(ctx, w.env.paths, ReasonAgent)
	if len(got) != 1 || got[0].Taken || !strings.Contains(got[0].Reason, "another checkpoint") {
		t.Fatalf("TakeNow with the gate held = %+v", got)
	}
	gate.Release()

	l, err := w.env.store(t).TryLock()
	if err != nil {
		t.Fatal(err)
	}
	got = w.s.TakeNow(t.Context(), w.env.paths, ReasonAgent)
	l.Unlock()
	if got[0].Taken || !strings.Contains(got[0].Reason, "lock is held") {
		t.Fatalf("TakeNow with the lock held = %+v", got)
	}
	if calls := w.take.calls(); len(calls) != 0 {
		t.Fatalf("took %v", calls)
	}
	// The gate is free again after each.
	if !gate.TryAcquire() {
		t.Fatal("TakeNow kept the gate")
	}
	gate.Release()
}

// An environment this daemon does not serve, or one whose path does not
// resolve, takes nothing.
func TestTakeNowRefusals(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.env.set(ServeNone, false)
	if got := w.s.TakeNow(t.Context(), w.env.paths, ReasonAgent); got[0].Taken || !strings.Contains(got[0].Reason, "does not serve") {
		t.Fatalf("unserved: %+v", got)
	}
	w.env.set(ServeHeld, true)
	w.env.mu.Lock()
	w.env.srcs, w.env.bad = nil, []Unresolved{{Path: w.env.paths[0], Reason: "no root holds it"}}
	w.env.mu.Unlock()
	if got := w.s.TakeNow(t.Context(), w.env.paths, ReasonAgent); got[0].Taken || !strings.Contains(got[0].Reason, "no root holds it") {
		t.Fatalf("unresolved: %+v", got)
	}
	if calls := w.take.calls(); len(calls) != 0 {
		t.Fatalf("took %v", calls)
	}
}

// A TakeNow held up by its own environment (a view that does not come
// back, as from a hung engine, or the environment's own tick) holds no
// gate: another environment's checkpoints go on. Waiting for the
// environment ends with the context.
func TestTakeNowHoldsNoGateWhileWaiting(t *testing.T) {
	gate := NewGate()
	w := newSchedWorld(t, gate)
	w.env.mu.Lock()
	w.env.viewing, w.env.unblock = make(chan struct{}), make(chan struct{})
	w.env.mu.Unlock()
	done := make(chan []NowResult, 1)
	go func() { done <- w.s.TakeNow(t.Context(), w.env.paths, ReasonAgent) }()
	<-w.env.viewing
	if !gate.TryAcquire() {
		t.Fatal("TakeNow holds the gate while it waits for the view")
	}
	gate.Release()
	w.env.mu.Lock()
	w.env.viewing = nil
	w.env.mu.Unlock()
	close(w.env.unblock)
	if got := <-done; !got[0].Taken {
		t.Fatalf("after the view came back: %+v", got)
	}

	// The environment's lock held (a tick of its own): the wait for it
	// ends with the context, the gate untouched.
	w.s.tick <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got := w.s.TakeNow(ctx, w.env.paths, ReasonAgent)
	<-w.s.tick
	if got[0].Taken || !strings.Contains(got[0].Reason, "busy") {
		t.Fatalf("with the environment busy: %+v", got)
	}
	if !gate.TryAcquire() {
		t.Fatal("the gate is held")
	}
	gate.Release()
}

// Below the soft free-space floor a checkpoint asked for now is skipped as
// the schedule's are: nothing taken, the reason says so, and status shows
// the skip.
func TestTakeNowBelowSoftFloor(t *testing.T) {
	w := newSchedWorld(t, nil)
	w.s.o.TakeOptions.Statfs = func(string) (uint64, uint64, error) { return 20 << 30, 1000 << 30, nil }
	got := w.s.TakeNow(t.Context(), w.env.paths[:1], ReasonAgent)
	if len(got) != 1 || got[0].Taken || !strings.Contains(got[0].Reason, "soft free-space floor") {
		t.Fatalf("TakeNow = %+v", got)
	}
	if es := w.env.list(t, w.key()); len(es) != 0 {
		t.Fatalf("store holds %v", es)
	}
	if st := w.s.Status(); !st.LowSpace || st.SpaceSkipped == "" || st.Paths[0].Failing != "" {
		t.Fatalf("status %+v", st)
	}
}
