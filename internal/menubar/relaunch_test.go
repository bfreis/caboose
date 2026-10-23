package menubar

import (
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// relaunchRig is a rig whose bundle an update replaces once replaced is
// set, counting its relaunches.
func relaunchRig(t *testing.T, fail error) (r *rig, replaced *atomic.Bool, relaunches *atomic.Int32) {
	replaced, relaunches = &atomic.Bool{}, &atomic.Int32{}
	r = newRig(t, func(o *Options) {
		o.Replaced = replaced.Load
		o.Relaunch = func() error { relaunches.Add(1); return fail }
	})
	return r, replaced, relaunches
}

func quitWithin(t *testing.T, r *rig) bool {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// An update that replaced the bundle: the app relaunches into it and
// quits, closing its connection first.
func TestRelaunchWhenReplaced(t *testing.T) {
	r, replaced, relaunches := relaunchRig(t, nil)
	r.start()
	r.advance(3 * ReplacedEvery)
	if relaunches.Load() != 0 {
		t.Fatal("relaunched with nothing replaced")
	}
	replaced.Store(true)
	r.advance(ReplacedEvery)
	if relaunches.Load() != 1 {
		t.Fatalf("relaunched %d times", relaunches.Load())
	}
	if !quitWithin(t, r) {
		t.Fatal("did not quit")
	}
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if !r.d.conns[0].closed {
		t.Error("quit without closing the connection")
	}
	if !strings.Contains(r.logText(), "relaunching into the new one") {
		t.Errorf("logged:\n%s", r.logText())
	}
}

// A relaunch that fails leaves the app running, says why once, and is
// not tried again.
func TestRelaunchFails(t *testing.T) {
	r, replaced, relaunches := relaunchRig(t, errors.New("open: no such bundle"))
	r.start()
	replaced.Store(true)
	r.advance(5 * ReplacedEvery)
	if relaunches.Load() != 1 {
		t.Errorf("relaunched %d times", relaunches.Load())
	}
	if quitWithin(t, r) {
		t.Error("quit though the relaunch failed")
	}
	if !strings.Contains(r.logText(), "cannot relaunch into the new one: open: no such bundle") {
		t.Errorf("logged:\n%s", r.logText())
	}
}

// Never while a URL question waits on the user: the relaunch waits for
// the answer.
func TestRelaunchWaitsForTheQuestion(t *testing.T) {
	r, replaced, relaunches := relaunchRig(t, nil)
	views := []daemonproto.RequestView{
		view(7, "open_url", "open_url:https://example.com", "open https://example.com/page in the browser", t0),
	}
	s := &requestsScript{views: map[string][]daemonproto.RequestView{"dev": views}, offers: map[uint64][]daemonproto.Offer{}}
	s.install(r.d)
	var shown atomic.Bool
	shown.Store(true)
	r.d.handle(daemonproto.MethodDialogs, func(json.RawMessage) (any, error) {
		if shown.Load() {
			return daemonproto.DialogsResult{Dialog: &daemonproto.DialogRef{Env: "dev", ID: 7}}, nil
		}
		return daemonproto.DialogsResult{}, nil
	})
	r.d.handle(daemonproto.MethodAnswer, func(json.RawMessage) (any, error) {
		shown.Store(false)
		return daemonproto.AnswerResult{}, nil
	})
	r.start()
	asks := r.ui.snapshot().asks
	if len(asks) != 1 {
		t.Fatalf("asks %+v", asks)
	}
	replaced.Store(true)
	r.advance(4 * ReplacedEvery)
	if relaunches.Load() != 0 {
		t.Fatal("relaunched with a question shown")
	}
	r.do(func(e Events) { e.URLAnswered(asks[0].ID, false) })
	r.advance(2 * ReplacedEvery)
	if relaunches.Load() != 1 {
		t.Errorf("relaunched %d times once answered", relaunches.Load())
	}
}

// No seams, no checks: an app with no way to relaunch never asks.
func TestRelaunchOff(t *testing.T) {
	var asked atomic.Bool
	r := newRig(t, func(o *Options) { o.Replaced = func() bool { asked.Store(true); return true } })
	r.start()
	r.advance(3 * ReplacedEvery)
	if asked.Load() {
		t.Error("asked with no Relaunch")
	}
}
