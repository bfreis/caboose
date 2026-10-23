package launcher

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/broker"
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

// nextAsked is the next dialog g shows, within a few seconds.
func (g *gateActions) nextAsked(t *testing.T) string {
	t.Helper()
	select {
	case p := <-g.asked:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no dialog")
	}
	return ""
}

// noDialog fails the test if this machine's dialog is shown.
type noDialog struct {
	linkActions
	t *testing.T
}

func (n *noDialog) Confirm(prompt string) (bool, error) {
	n.t.Errorf("a dialog was shown: %q", prompt)
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

// testQueue is a dialog queue that ends with the test.
func testQueue(t *testing.T) *dialogQueue {
	q := newDialogQueue()
	t.Cleanup(q.close)
	return q
}

// A daemon that goes away while this machine's dialog shows a request
// leaves the mark in requests.json, and the next broker denies the
// request as it opens, before anyone is served.
func TestDialogsInFlightDeniedAtStart(t *testing.T) {
	app, _ := brokerEnv(t, "[link]\nopen_urls = \"ask\"\n")
	g := newGateActions()
	r, stop := queueRunner(t, app, g, testQueue(t))
	c := fileURL(t, r, "https://c.example/three")
	if p := g.nextAsked(t); !strings.Contains(p, "c.example") {
		t.Fatalf("asked %q", p)
	}
	stop()
	g.answers <- true
	r2, _ := queueRunner(t, app, &noDialog{t: t}, testQueue(t))
	qi, _ := r2.broker.Request(c)
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

// The queue is one across environments, shown one at a time in the order
// filed; a link that stops takes its queued dialogs with it.
func TestDialogQueueOrder(t *testing.T) {
	q := testQueue(t)
	g := newGateActions()
	alpha := &urlDialog{actions: g, answers: &queueAnswers{}, logf: t.Logf, queue: q}
	beta := &urlDialog{actions: g, answers: &queueAnswers{}, logf: t.Logf, queue: q}
	alpha.Ask("alpha", 1, "https://a.example/")
	beta.Ask("beta", 1, "https://b.example/")
	alpha.Ask("alpha", 2, "https://a.example/2")
	beta.Ask("beta", 2, "https://b.example/2")
	if p := g.nextAsked(t); !strings.Contains(p, "environment alpha asks") || !strings.HasSuffix(p, "\n\nhttps://a.example/") {
		t.Fatalf("first: %q", p)
	}
	beta.close()
	g.answers <- true
	if p := g.nextAsked(t); !strings.HasSuffix(p, "\n\nhttps://a.example/2") {
		t.Fatalf("after beta stopped: %q", p)
	}
	g.answers <- true
	waitUntil(t, func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return !q.running && len(q.items) == 0
	})
	select {
	case p := <-g.asked:
		t.Fatalf("a stopped link's dialog was shown: %q", p)
	default:
	}
}

// queueAnswers is a broker of pending requests, by number.
type queueAnswers struct {
	mu   sync.Mutex
	done map[uint64]bool
}

func (a *queueAnswers) Request(id uint64) (broker.RequestInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done[id] {
		return broker.RequestInfo{ID: id, State: broker.StateAllowed}, nil
	}
	return broker.RequestInfo{ID: id, State: broker.StatePending}, nil
}

func (a *queueAnswers) Answer(id uint64, _ broker.Reply) (broker.AnswerResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done == nil {
		a.done = map[uint64]bool{}
	}
	a.done[id] = true
	return broker.AnswerResult{}, nil
}

func (a *queueAnswers) MarkInFlight(uint64, string) error { return nil }

func readRequests(t *testing.T, app *App) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(app.Cfg.EnvDir, "requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// markFails is a broker's answers whose in-flight marks fail, as a disk
// that cannot write requests.json does.
type markFails struct{}

func (markFails) Request(id uint64) (broker.RequestInfo, error) {
	return broker.RequestInfo{ID: id, State: broker.StatePending}, nil
}

func (markFails) Answer(uint64, broker.Reply) (broker.AnswerResult, error) {
	return broker.AnswerResult{}, nil
}

func (markFails) MarkInFlight(uint64, string) error { return io.ErrShortWrite }

// A request whose in-flight mark cannot be recorded is not shown, since
// a daemon going away would leave it to be asked again: it stays
// pending, and the failure is logged.
func TestDialogsUnmarkedNotShown(t *testing.T) {
	var logs []string
	d := &urlDialog{actions: &noDialog{t: t}, answers: markFails{}, logf: func(f string, a ...any) {
		logs = append(logs, f)
	}, queue: testQueue(t)}
	d.show("dev", 6, "https://c.example/")
	if len(logs) != 1 {
		t.Errorf("logged %q", logs)
	}
}

// A closed queue forgets what was queued, and shows nothing pushed
// after.
func TestDialogQueueClose(t *testing.T) {
	q := newDialogQueue()
	g := newGateActions()
	d := &urlDialog{answers: &queueAnswers{}, logf: t.Logf, queue: q, actions: g}
	d.Ask("dev", 1, "https://a.example/")
	g.nextAsked(t)
	d.Ask("dev", 2, "https://b.example/")
	q.close()
	g.answers <- false
	d.Ask("dev", 3, "https://c.example/")
	waitUntil(t, func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return !q.running
	})
	q.mu.Lock()
	queued := len(q.items)
	q.mu.Unlock()
	if queued != 0 {
		t.Fatalf("after close: queued %d", queued)
	}
	select {
	case p := <-g.asked:
		t.Fatalf("shown after close: %q", p)
	default:
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
