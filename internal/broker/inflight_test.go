package broker

import (
	"strings"
	"testing"
)

// A request marked in flight when its broker went away is denied as the
// next broker opens, before anyone is served: by the dialog that showed
// it, scope once, with the reason that says to ask again. One not marked,
// or unmarked again, stays pending.
func TestInFlightDeniedAtOpen(t *testing.T) {
	f := newFixture(t)
	dlg := mustPending(t, f.decide(urlReq("https://dialog.example/x")))
	queued := mustPending(t, f.decide(urlReq("https://queued.example/x")))
	gone := mustPending(t, f.decide(urlReq("https://unmarked.example/x")))
	for _, id := range []uint64{dlg, gone} {
		if err := f.b.MarkInFlight(id, ByDialog); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.b.MarkInFlight(gone, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.read(requestsFile)), `"in_flight": "dialog"`) {
		t.Fatalf("the mark is not in requests.json:\n%s", f.read(requestsFile))
	}
	f.reopen()
	ri, err := f.b.Request(dlg)
	if err != nil || ri.State != StateDenied || ri.DecidedBy != ByDialog || ri.Reason != ReasonDialogGone || ri.Scope != ScopeOnce {
		t.Errorf("#%d after the restart: %+v %v", dlg, ri, err)
	}
	for _, id := range []uint64{queued, gone} {
		if ri, _ := f.b.Request(id); ri.State != StatePending {
			t.Errorf("#%d, never in flight, was decided: %+v", id, ri)
		}
	}
	if strings.Contains(string(f.read(requestsFile)), "in_flight") {
		t.Fatalf("a decided request keeps its mark:\n%s", f.read(requestsFile))
	}
	if !strings.Contains(string(f.read(decisionsFile)), ReasonDialogGone) {
		t.Error("decisions.log does not say why")
	}
}

// Only the dialog marks a request in flight, and a mark on a decided
// request changes nothing.
func TestMarkInFlight(t *testing.T) {
	f := newFixture(t)
	id := mustPending(t, f.decide(urlReq("https://example.com/x")))
	for _, by := range []string{ByCLI} {
		if err := f.b.MarkInFlight(id, by); err == nil {
			t.Fatalf("a mark by %s", by)
		}
	}
	f.answer(id, Reply{Verdict: ReplyDeny, Scope: ScopeOnce, By: ByCLI})
	if err := f.b.MarkInFlight(id, ByDialog); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if ri, _ := f.b.Request(id); ri.DecidedBy != ByCLI || ri.Reason == ReasonDialogGone {
		t.Fatalf("%+v", ri)
	}
}
