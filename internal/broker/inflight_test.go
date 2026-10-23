package broker

import (
	"errors"
	"strings"
	"testing"
)

// A request marked in flight when its broker went away is denied as the
// next broker opens, before anyone is served: by whoever showed it, scope
// once, with the reason that says to ask again. One not marked, or
// unmarked again, stays pending.
func TestInFlightDeniedAtOpen(t *testing.T) {
	f := newFixture(t)
	app := mustPending(t, f.decide(urlReq("https://app.example/x")))
	dlg := mustPending(t, f.decide(urlReq("https://dialog.example/x")))
	queued := mustPending(t, f.decide(urlReq("https://queued.example/x")))
	gone := mustPending(t, f.decide(urlReq("https://unmarked.example/x")))
	for id, by := range map[uint64]string{app: ByApp, dlg: ByDialog, gone: ByDialog} {
		if err := f.b.MarkInFlight(id, by); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.b.MarkInFlight(gone, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.read(requestsFile)), `"in_flight": "app"`) {
		t.Fatalf("the mark is not in requests.json:\n%s", f.read(requestsFile))
	}
	f.reopen()
	for id, want := range map[uint64][2]string{app: {ByApp, ReasonAppGone}, dlg: {ByDialog, ReasonDialogGone}} {
		ri, err := f.b.Request(id)
		if err != nil || ri.State != StateDenied || ri.DecidedBy != want[0] || ri.Reason != want[1] || ri.Scope != ScopeOnce {
			t.Errorf("#%d after the restart: %+v %v", id, ri, err)
		}
	}
	for _, id := range []uint64{queued, gone} {
		if ri, _ := f.b.Request(id); ri.State != StatePending {
			t.Errorf("#%d, never in flight, was decided: %+v", id, ri)
		}
	}
	if strings.Contains(string(f.read(requestsFile)), "in_flight") {
		t.Fatalf("a decided request keeps its mark:\n%s", f.read(requestsFile))
	}
	if !strings.Contains(string(f.read(decisionsFile)), ReasonAppGone) {
		t.Error("decisions.log does not say why")
	}
}

// DenyInFlight denies a pending request once, by whom it names; an
// answer after it is already_answered, and it refuses another answerer.
func TestDenyInFlight(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(urlReq("https://example.com/x")))
	if err := f.b.MarkInFlight(id, ByCLI); err == nil {
		t.Fatal("a mark by the cli")
	}
	if err := f.b.DenyInFlight(id, ByWeb); err == nil {
		t.Fatal("an in-flight denial by the page")
	}
	if err := f.b.MarkInFlight(id, ByApp); err != nil {
		t.Fatal(err)
	}
	if err := f.b.DenyInFlight(id, ByApp); err != nil {
		t.Fatal(err)
	}
	ri, _ := f.b.Request(id)
	if ri.State != StateDenied || ri.DecidedBy != ByApp || ri.Reason != ReasonAppGone || ri.Scope != ScopeOnce {
		t.Fatalf("%+v", ri)
	}
	if _, err := f.b.Answer(id, Reply{Verdict: ReplyAllow, Scope: ScopeOnce, By: ByApp}); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("a late answer: %v", err)
	}
	if err := f.b.DenyInFlight(id, ByApp); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("denied twice: %v", err)
	}
	if len(f.open.opened()) != 0 {
		t.Fatal("opened")
	}
	// A mark on a decided request changes nothing.
	if err := f.b.MarkInFlight(id, ByApp); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if ri, _ := f.b.Request(id); ri.DecidedBy != ByApp || ri.Reason != ReasonAppGone {
		t.Fatalf("%+v", ri)
	}
}
