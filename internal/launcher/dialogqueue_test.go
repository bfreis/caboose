package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/policy"
)

// gateActions is this machine's dialog, faked: each Confirm says what
// it asks on asked and waits for its answer on answers; what is opened
// is recorded.
type gateActions struct {
	linkActions
	asked   chan string
	answers chan bool
}

func newGateActions() *gateActions {
	return &gateActions{asked: make(chan string, 16), answers: make(chan bool)}
}

func (g *gateActions) Confirm(prompt string) (bool, error) {
	g.asked <- prompt
	return <-g.answers, nil
}

// noDialog fails the test if this machine's dialog is shown.
type noDialog struct {
	linkActions
	t *testing.T
}

func (n *noDialog) Confirm(prompt string) (bool, error) {
	n.t.Errorf("this machine's dialog was shown while the app holds the queue: %q", prompt)
	return false, nil
}

// queueRunner is a link runner for app, holding its link.lock, whose
// URL dialogs go to q; stop closes its broker and lets go of the lock,
// as a link that stops (or a daemon that goes away) does.
func queueRunner(t *testing.T, app *App, actions interface {
	OpenURL(string) error
	Notify(string, string) error
	Confirm(string) (bool, error)
}, q *dialogQueue) (r *linkRunner, stop func()) {
	t.Helper()
	lock, unlock, err := lockLinkFile(app.Cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err = app.newLinkRunner(linkRun{out: io.Discard, lock: lock, actions: actions, dialogs: q})
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	var once sync.Once
	stop = func() { once.Do(func() { r.closeBroker(); unlock() }) }
	t.Cleanup(stop)
	return r, stop
}

func fileURL(t *testing.T, r *linkRunner, u string) uint64 {
	t.Helper()
	i := strings.Index(u[len("https://"):], "/")
	o, err := r.cfg.Decide.Decide(policy.Request{Kind: policy.KindOpenURL, URL: u, Origin: u[:len("https://")+i]})
	if err != nil || o.Verdict != broker.Pending {
		t.Fatalf("filing %s: %+v %v", u, o, err)
	}
	return o.Request
}

func next(t *testing.T, q *dialogQueue, n uint64) *daemonproto.DialogRef {
	t.Helper()
	d, err := q.Next(n)
	if err != nil {
		t.Fatalf("dialogs: %v", err)
	}
	return d
}

// daemonOf serves r's broker as the daemon's Brokers do.
func daemonOf(r *linkRunner) *DaemonLinks {
	return &DaemonLinks{brokerHook: func(string) *brokerHandle { return &brokerHandle{b: r.broker} }}
}

// With an app holding dialogs, the queue shows nothing on this machine:
// the app takes each request from dialogs, one at a time and in the
// order filed, and gets the one in flight again until it is answered.
// The app's allow is recorded as the app's whatever its params say, and
// opens the URL the host stored, never one from the params; a late
// answer of the app's is already_answered.
func TestDialogsToTheApp(t *testing.T) {
	app, _ := brokerEnv(t, "[link]\nopen_urls = \"ask\"\n")
	q := testQueue(t)
	nd := &noDialog{t: t}
	r, _ := queueRunner(t, app, nd, q)
	q.SetApp(1, true)
	env := app.Cfg.Env
	a := fileURL(t, r, "https://a.example/one")
	b := fileURL(t, r, "https://b.example/two")
	c := fileURL(t, r, "https://c.example/three")

	if d := next(t, q, 1); d == nil || d.ID != a || d.Env != env {
		t.Fatalf("first: %+v", d)
	}
	if d := next(t, q, 1); d == nil || d.ID != a {
		t.Fatalf("the one in flight again: %+v", d)
	}
	if _, err := q.Next(2); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Fatalf("another attachment: %v", err)
	}
	if !strings.Contains(string(readRequests(t, app)), `"in_flight": "app"`) {
		t.Fatal("the request in flight is not marked so in requests.json")
	}

	l := daemonOf(r)
	params := json.RawMessage(`{"env":"` + env + `","id":` + itoa(a) + `,"verdict":"allow","scope":"once","decided_by":"cli","by":"cli","url":"https://evil.example/"}`)
	res, err := l.Call(t.Context(), daemonproto.ClientApp, daemonproto.MethodAnswer, env, params)
	if err != nil {
		t.Fatal(err)
	}
	if v := res.(*daemonproto.AnswerResult); v.Request.DecidedBy != broker.ByApp || v.Request.State != broker.StateAllowed {
		t.Fatalf("the app's answer: %+v", v.Request)
	}
	if opened, _ := nd.state(); !slices.Equal(opened, []string{"https://a.example/one"}) {
		t.Fatalf("opened %v", opened)
	}
	if d := next(t, q, 1); d == nil || d.ID != b {
		t.Fatalf("after the answer: %+v", d)
	}
	// The command line answers the one in flight: dialogs no longer has
	// it (the app dismisses its alert), and the app's late answer is
	// dropped.
	if _, err := r.broker.Answer(b, broker.Reply{Verdict: broker.ReplyDeny, Scope: broker.ScopeOnce, By: broker.ByCLI}); err != nil {
		t.Fatal(err)
	}
	if d := next(t, q, 1); d == nil || d.ID != c {
		t.Fatalf("after the CLI's answer: %+v", d)
	}
	late := json.RawMessage(`{"env":"` + env + `","id":` + itoa(b) + `,"verdict":"allow","scope":"once"}`)
	if _, err := l.Call(t.Context(), daemonproto.ClientApp, daemonproto.MethodAnswer, env, late); daemonproto.Kind(err) != daemonproto.KindAlreadyAnswered {
		t.Fatalf("a late answer: %v", err)
	}
	if qi, _ := r.broker.Request(b); qi.DecidedBy != broker.ByCLI || qi.State != broker.StateDenied {
		t.Fatalf("the CLI's answer was overturned: %+v", qi)
	}
	if opened, _ := nd.state(); len(opened) != 1 {
		t.Fatalf("opened %v", opened)
	}
	// Answered by the app, the queue is empty.
	if _, err := l.Call(t.Context(), daemonproto.ClientApp, daemonproto.MethodAnswer, env,
		json.RawMessage(`{"env":"`+env+`","id":`+itoa(c)+`,"verdict":"deny"}`)); err != nil {
		t.Fatal(err)
	}
	if d := next(t, q, 1); d != nil {
		t.Fatalf("an empty queue: %+v", d)
	}
}

// The attachment ending with a request in flight denies it -- by the
// app, scope once, "the app went away; ask again" -- and never asks it
// again; the requests still queued go to this machine's dialog, in
// order.
func TestDialogsAppGoesAway(t *testing.T) {
	app, _ := brokerEnv(t, "[link]\nopen_urls = \"ask\"\n")
	q := testQueue(t)
	g := newGateActions()
	r, _ := queueRunner(t, app, g, q)
	q.SetApp(1, true)
	a := fileURL(t, r, "https://a.example/one")
	b := fileURL(t, r, "https://b.example/two")
	c := fileURL(t, r, "https://c.example/three")
	if d := next(t, q, 1); d == nil || d.ID != a {
		t.Fatalf("first: %+v", d)
	}
	// A late off of another attachment changes nothing.
	q.SetApp(7, false)
	if d := next(t, q, 1); d == nil || d.ID != a {
		t.Fatalf("after another's off: %+v", d)
	}
	q.SetApp(1, false)
	qi, _ := r.broker.Request(a)
	if qi.State != broker.StateDenied || qi.DecidedBy != broker.ByApp || qi.Scope != broker.ScopeOnce || qi.Reason != broker.ReasonAppGone {
		t.Fatalf("the request in flight: %+v", qi)
	}
	for _, want := range []string{"https://b.example/two", "https://c.example/three"} {
		select {
		case p := <-g.asked:
			if !strings.Contains(p, want) {
				t.Fatalf("asked %q, want %s", p, want)
			}
			if !strings.Contains(string(readRequests(t, app)), `"in_flight": "dialog"`) {
				t.Error("this machine's dialog is not marked in flight")
			}
			g.answers <- false
		case <-time.After(5 * time.Second):
			t.Fatalf("no dialog for %s", want)
		}
	}
	waitUntil(t, func() bool { qi, _ := r.broker.Request(c); return qi.State == broker.StateDenied })
	for _, id := range []uint64{b, c} {
		if qi, _ := r.broker.Request(id); qi.DecidedBy != broker.ByDialog {
			t.Errorf("#%d: %+v", id, qi)
		}
	}
	if _, err := q.Next(1); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Fatalf("dialogs after the app went away: %v", err)
	}
}

// A daemon that goes away with a request in flight leaves the mark in
// requests.json, and the next broker denies the request as it opens,
// before anyone is served: the app's, and this machine's dialog's alike.
func TestDialogsInFlightDeniedAtStart(t *testing.T) {
	app, _ := brokerEnv(t, "[link]\nopen_urls = \"ask\"\n")
	q := testQueue(t)
	r, stop := queueRunner(t, app, &noDialog{t: t}, q)
	q.SetApp(1, true)
	a := fileURL(t, r, "https://a.example/one")
	b := fileURL(t, r, "https://b.example/two")
	if d := next(t, q, 1); d == nil || d.ID != a {
		t.Fatalf("first: %+v", d)
	}
	stop()

	g := newGateActions()
	q2 := testQueue(t)
	r2, stop2 := queueRunner(t, app, g, q2)
	qi, _ := r2.broker.Request(a)
	if qi.State != broker.StateDenied || qi.DecidedBy != broker.ByApp || qi.Reason != broker.ReasonAppGone {
		t.Fatalf("in flight at the restart: %+v", qi)
	}
	if qi, _ := r2.broker.Request(b); qi.State != broker.StatePending {
		t.Fatalf("queued at the restart: %+v", qi)
	}

	// This machine's dialog, showing as the link stops.
	c := fileURL(t, r2, "https://c.example/three")
	select {
	case p := <-g.asked:
		if !strings.Contains(p, "c.example") {
			t.Fatalf("asked %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no dialog")
	}
	stop2()
	g.answers <- true
	r3, _ := queueRunner(t, app, &noDialog{t: t}, testQueue(t))
	qi, _ = r3.broker.Request(c)
	if qi.State != broker.StateDenied || qi.DecidedBy != broker.ByDialog || qi.Reason != broker.ReasonDialogGone {
		t.Fatalf("this machine's dialog at the restart: %+v", qi)
	}
	if opened, _ := g.state(); len(opened) != 0 {
		t.Fatalf("opened %v", opened)
	}
}

// A dialog that cannot be shown leaves no mark: the request stays
// pending, also for the next broker.
func TestDialogFailureLeavesNoMark(t *testing.T) {
	app, _ := brokerEnv(t, "[link]\nopen_urls = \"ask\"\n")
	actions := &linkActions{failAsk: true}
	r, stop := queueRunner(t, app, actions, testQueue(t))
	a := fileURL(t, r, "https://a.example/one")
	waitUntil(t, func() bool { _, asked := actions.state(); return len(asked) == 1 })
	waitUntil(t, func() bool { return !strings.Contains(string(readRequests(t, app)), "in_flight") })
	stop()
	r2, _ := queueRunner(t, app, &noDialog{t: t}, testQueue(t))
	if qi, _ := r2.broker.Request(a); qi.State != broker.StatePending {
		t.Fatalf("%+v", qi)
	}
}

// The queue is one across environments, in the order filed; a link that
// stops takes its queued dialogs with it, and one in flight with the app
// whose link stopped is skipped.
func TestDialogQueueOrder(t *testing.T) {
	q := testQueue(t)
	q.SetApp(1, true)
	alpha := &urlDialog{answers: &queueAnswers{}, logf: t.Logf, queue: q}
	beta := &urlDialog{answers: &queueAnswers{}, logf: t.Logf, queue: q}
	alpha.Ask("alpha", 1, "https://a.example/")
	beta.Ask("beta", 1, "https://b.example/")
	alpha.Ask("alpha", 2, "https://a.example/2")
	beta.Ask("beta", 2, "https://b.example/2")
	if d := next(t, q, 1); *d != (daemonproto.DialogRef{Env: "alpha", ID: 1}) {
		t.Fatalf("%+v", d)
	}
	alpha.answers.(*queueAnswers).answer(1)
	if d := next(t, q, 1); *d != (daemonproto.DialogRef{Env: "beta", ID: 1}) {
		t.Fatalf("%+v", d)
	}
	beta.close()
	if d := next(t, q, 1); *d != (daemonproto.DialogRef{Env: "alpha", ID: 2}) {
		t.Fatalf("after beta stopped: %+v", d)
	}
	// A second attachment taking the queue (its predecessor's detach
	// still to come) denies the predecessor's request in flight.
	q.SetApp(2, true)
	if got := alpha.answers.(*queueAnswers).denied(); !slices.Equal(got, []uint64{2}) {
		t.Fatalf("denied %v", got)
	}
	if d := next(t, q, 2); d != nil {
		t.Fatalf("beta's queued one is gone: %+v", d)
	}
	if got := beta.answers.(*queueAnswers).denied(); len(got) != 0 {
		t.Fatalf("a stopped link denied %v", got)
	}
}

// While an app holds the queue, a dialog pushed starts nothing that
// would show it on this machine; once the app lets go, it does. Checked
// on the queue's state, not by waiting for a dialog that should not come.
func TestDialogQueueHeldStartsNothing(t *testing.T) {
	q := testQueue(t)
	q.SetApp(1, true)
	d := &urlDialog{answers: &queueAnswers{}, logf: t.Logf, queue: q}
	q.push(&dialogItem{d: d, env: "alpha", id: 1, url: "https://a.example/"})
	q.mu.Lock()
	running, queued := q.running, len(q.items)
	q.mu.Unlock()
	if running || queued != 1 {
		t.Fatalf("held by the app: running %v, queued %d", running, queued)
	}
	q.mu.Lock()
	q.kick()
	running = q.running
	q.mu.Unlock()
	if running {
		t.Fatal("kick started this machine's dialogs while the app holds the queue")
	}
}

// queueAnswers is a broker of pending requests, by number.
type queueAnswers struct {
	mu   sync.Mutex
	done map[uint64]bool
	den  []uint64
}

func (a *queueAnswers) answer(id uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done == nil {
		a.done = map[uint64]bool{}
	}
	a.done[id] = true
}

func (a *queueAnswers) denied() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.den)
}

func (a *queueAnswers) Request(id uint64) (broker.RequestInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done[id] {
		return broker.RequestInfo{ID: id, State: broker.StateAllowed}, nil
	}
	return broker.RequestInfo{ID: id, State: broker.StatePending}, nil
}

func (a *queueAnswers) Answer(uint64, broker.Reply) (broker.AnswerResult, error) {
	return broker.AnswerResult{}, nil
}

func (a *queueAnswers) MarkInFlight(uint64, string) error { return nil }

func (a *queueAnswers) DenyInFlight(id uint64, by string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.den = append(a.den, id)
	return nil
}

func readRequests(t *testing.T, app *App) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(app.Cfg.EnvDir, "requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func itoa(n uint64) string { return strconv.FormatUint(n, 10) }

// markFails is a broker's answers whose in-flight marks fail until ok,
// as a disk that cannot write requests.json does.
type markFails struct {
	mu    sync.Mutex
	ok    bool
	marks []string
}

func (m *markFails) Request(id uint64) (broker.RequestInfo, error) {
	return broker.RequestInfo{ID: id, State: broker.StatePending}, nil
}

func (m *markFails) Answer(uint64, broker.Reply) (broker.AnswerResult, error) {
	return broker.AnswerResult{}, nil
}

func (m *markFails) MarkInFlight(id uint64, by string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ok {
		return io.ErrShortWrite
	}
	m.marks = append(m.marks, strconv.FormatUint(id, 10)+" "+by)
	return nil
}

func (m *markFails) DenyInFlight(uint64, string) error { return nil }

// A request whose in-flight mark cannot be recorded is shown by no one,
// since a daemon going away would leave it to be asked again: the app
// is handed nothing, the request waits at the front of the queue, and it
// is handed out once the mark holds. This machine's dialog shows none.
func TestDialogsUnmarkedNotShown(t *testing.T) {
	ans := &markFails{}
	var logs []string
	var mu sync.Mutex
	d := &urlDialog{actions: &noDialog{t: t}, answers: ans, logf: func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, f)
	}}
	q := testQueue(t)
	d.queue = q
	q.SetApp(1, true)
	d.Ask("dev", 4, "https://a.example/")
	d.Ask("dev", 5, "https://b.example/")
	// After each failed mark the request is back at the front, ahead of
	// the one filed after it.
	for i := range 2 {
		if r := next(t, q, 1); r != nil {
			t.Fatalf("handed out unmarked: %+v", r)
		}
		q.mu.Lock()
		ids := []uint64{}
		for _, it := range q.items {
			ids = append(ids, it.id)
		}
		q.mu.Unlock()
		if !slices.Equal(ids, []uint64{4, 5}) {
			t.Fatalf("after failed mark %d, queued %v, want [4 5]", i+1, ids)
		}
	}
	if len(q.items) != 2 || q.items[0].id != 4 || q.inFlight != nil {
		t.Fatalf("queue after a failed mark: %d items, in flight %v", len(q.items), q.inFlight)
	}
	mu.Lock()
	if len(logs) != 2 {
		t.Errorf("logged %q", logs)
	}
	mu.Unlock()
	ans.mu.Lock()
	ans.ok = true
	ans.mu.Unlock()
	if r := next(t, q, 1); r == nil || r.ID != 4 {
		t.Fatalf("once the mark holds: %+v", r)
	}
	if !slices.Equal(ans.marks, []string{"4 " + broker.ByApp}) {
		t.Errorf("marks %q", ans.marks)
	}
	// This machine's dialog does not show an unmarked request either.
	ans.mu.Lock()
	ans.ok = false
	ans.mu.Unlock()
	d.show("dev", 6, "https://c.example/")
}

// fakeClock is a dialogQueue's clock that moves only when advanced, and
// runs the timers due then, outside its lock.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) afterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	tm := &fakeTimer{at: c.t.Add(d), f: f}
	c.timers = append(c.timers, tm)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !tm.stopped
		tm.stopped = true
		return was
	}
}

// advance moves the clock by d and runs every timer due, in order.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	var due []*fakeTimer
	c.timers = slices.DeleteFunc(c.timers, func(tm *fakeTimer) bool {
		if tm.stopped || tm.at.After(c.t) {
			return tm.stopped
		}
		due = append(due, tm)
		return true
	})
	c.mu.Unlock()
	for _, tm := range due {
		tm.f()
	}
}

// pending is how many timers are set and not stopped.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, tm := range c.timers {
		if !tm.stopped {
			n++
		}
	}
	return n
}

func clockedQueue() (*dialogQueue, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	q := newDialogQueue()
	q.now, q.afterFunc = c.now, c.afterFunc
	return q, c
}

// An app that holds the queue and stops polling is let go of after
// dialogAppStale, as at its detach: the request in flight with it is
// denied, the queued ones go back to this machine's dialog, and the app
// is refused dialogs from then on. Polls in time keep it, however long
// it holds the queue, and a timer stopped as the app changes does
// nothing.
func TestDialogsAppStopsPolling(t *testing.T) {
	q, c := clockedQueue()
	ans := &queueAnswers{}
	d := &urlDialog{actions: &noDialog{t: t}, answers: ans, logf: t.Logf, queue: q}
	q.SetApp(1, true)
	d.Ask("dev", 1, "https://a.example/")
	d.Ask("dev", 2, "https://b.example/")
	if r := next(t, q, 1); r == nil || r.ID != 1 {
		t.Fatalf("first: %+v", r)
	}
	// Polling in time, for many times the bound.
	for range 20 {
		c.advance(dialogAppStale - time.Second)
		if r := next(t, q, 1); r == nil || r.ID != 1 {
			t.Fatalf("polled in time: %+v", r)
		}
	}
	if n := c.pending(); n != 1 {
		t.Fatalf("%d timers set, want 1", n)
	}
	if got := ans.denied(); len(got) != 0 {
		t.Fatalf("denied %v while the app polls", got)
	}
	c.advance(dialogAppStale - time.Second)
	if got := ans.denied(); len(got) != 0 {
		t.Fatalf("denied %v before the bound", got)
	}
	q.mu.Lock()
	held := q.app
	q.mu.Unlock()
	if held != 1 {
		t.Fatalf("let go of before the bound: app %d", held)
	}
	// Ending the noDialog check: from here, this machine's dialog may
	// show what is queued; the test takes it off the queue first.
	d.actions = &linkActions{}
	c.advance(time.Second)
	if got := ans.denied(); !slices.Equal(got, []uint64{1}) {
		t.Fatalf("denied %v, want the one in flight", got)
	}
	q.mu.Lock()
	held, inFlight := q.app, q.inFlight
	q.mu.Unlock()
	if held != 0 || inFlight != nil {
		t.Fatalf("after the bound: app %d, in flight %v", held, inFlight)
	}
	if _, err := q.Next(1); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Fatalf("the stale app's next poll: %v", err)
	}
	if n := c.pending(); n != 0 {
		t.Fatalf("%d timers set with no app", n)
	}
}

// A timer set for one attachment and stopped too late changes nothing
// once another holds the queue (nor sets a second timer), and a late off
// of the stale one neither.
func TestDialogsStaleTimerOfAnotherApp(t *testing.T) {
	q, c := clockedQueue()
	ans := &queueAnswers{}
	d := &urlDialog{actions: &noDialog{t: t}, answers: ans, logf: t.Logf, queue: q}
	q.SetApp(1, true)
	q.mu.Lock()
	old := q.timerGen
	q.mu.Unlock()
	c.advance(dialogAppStale / 2)
	q.SetApp(2, true)
	d.Ask("dev", 1, "https://a.example/")
	if r := next(t, q, 2); r == nil || r.ID != 1 {
		t.Fatalf("first: %+v", r)
	}
	q.checkStale(old) // the first attachment's, fired as it was stopped
	if n := c.pending(); n != 1 {
		t.Fatalf("%d timers set after a stale one fired, want 1", n)
	}
	c.advance(dialogAppStale / 2)
	if got := ans.denied(); len(got) != 0 {
		t.Fatalf("denied %v by another attachment's timer", got)
	}
	q.SetApp(1, false)
	if r := next(t, q, 2); r == nil || r.ID != 1 {
		t.Fatalf("still in flight with the app that holds it: %+v", r)
	}
}

// testQueue is a dialog queue with time's clock that ends with the test:
// its staleness timer, real here, never fires into a later test.
func testQueue(t *testing.T) *dialogQueue {
	q := newDialogQueue()
	t.Cleanup(q.close)
	return q
}

// An app that holds the queue goes stale with a dialog queued whose link
// has no way to ask on this machine: the dialog goes back to this
// machine, shows nothing, and the request stays pending, unmarked. (A
// test's queue left with such a dialog and a real timer once crashed a
// later test this way, 30 s on.)
func TestDialogStaleWithoutActions(t *testing.T) {
	q, c := clockedQueue()
	ans := &markAnswers{}
	var logged strings.Builder
	var mu sync.Mutex
	d := &urlDialog{answers: ans, queue: q, logf: func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&logged, f+"\n", a...)
	}}
	q.SetApp(1, true)
	d.Ask("dev", 1, "https://a.example/")
	c.advance(dialogAppStale)
	waitUntil(t, func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return !q.running && len(q.items) == 0
	})
	if got := ans.marked(); len(got) != 0 {
		t.Fatalf("marked %v with no way to ask", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logged.String(), "request #1: no dialog on this machine") {
		t.Fatalf("logged %q", logged.String())
	}
}

// markAnswers is queueAnswers that records its marks.
type markAnswers struct {
	queueAnswers
	marks []uint64
}

func (a *markAnswers) MarkInFlight(id uint64, by string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.marks = append(a.marks, id)
	return nil
}

func (a *markAnswers) marked() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.marks)
}

// A closed queue sets no timer, forgets what was queued, and shows
// nothing pushed after, whoever holds it.
func TestDialogQueueClose(t *testing.T) {
	q, c := clockedQueue()
	d := &urlDialog{answers: &queueAnswers{}, logf: t.Logf, queue: q, actions: &noDialog{t: t}}
	q.SetApp(1, true)
	d.Ask("dev", 1, "https://a.example/")
	q.close()
	if n := c.pending(); n != 0 {
		t.Fatalf("%d timers set after close", n)
	}
	c.advance(dialogAppStale)
	q.SetApp(1, false) // let go: would kick this machine's dialog
	d.Ask("dev", 2, "https://b.example/")
	q.SetApp(2, true)
	if n := c.pending(); n != 0 {
		t.Fatalf("%d timers set by an app after close", n)
	}
	q.mu.Lock()
	running, queued := q.running, len(q.items)
	q.mu.Unlock()
	if running || queued != 0 {
		t.Fatalf("after close: running %v, queued %d", running, queued)
	}
}

// A link's own queue ends with its broker; one the daemon's links share
// ends only with the daemon's links.
func TestDialogQueueClosedWithItsOwner(t *testing.T) {
	app, _ := brokerEnv(t, "[link]\nopen_urls = \"ask\"\n")
	lock, unlock, err := lockLinkFile(app.Cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	r, err := app.newLinkRunner(linkRun{out: io.Discard, lock: lock, actions: &noDialog{t: t}})
	if err != nil {
		t.Fatal(err)
	}
	own := r.dialog.queue
	r.closeBroker()
	if !own.closed {
		t.Fatal("a link's own queue outlived its broker")
	}
	unlock()

	l := NewDaemonLinks(DaemonLinksOptions{Dir: filepath.Join(t.TempDir(), "daemon"), CabooseHome: t.TempDir(), NewApp: func(string) (*App, error) {
		return nil, errors.New("not asked")
	}})
	lock, unlock, err = lockLinkFile(app.Cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	r, err = app.newLinkRunner(linkRun{out: io.Discard, lock: lock, actions: &noDialog{t: t}, dialogs: l.dialogs})
	if err != nil {
		t.Fatal(err)
	}
	r.closeBroker()
	if l.dialogs.closed {
		t.Fatal("one link's stop closed the daemon's shared queue")
	}
	l.Close()
	if !l.dialogs.closed {
		t.Fatal("the daemon's queue outlived its links")
	}
}
