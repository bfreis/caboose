package launcher

import (
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
)

// dialogQueue is the URL dialogs of every link in this process (the
// daemon's, or a helper's one link), shown one at a time in the order
// their requests were filed: by this machine's own dialog
// (urlDialog.show), or, while a menu-bar app holding dialogs is
// attached, by the app, which takes them one at a time with the dialogs
// method (daemon.Dialogs). Whoever shows a request's dialog marks it in
// flight in requests.json first; the request is denied, never asked
// again, if they go away before it is answered. An app that holds the
// queue and stops polling for dialogAppStale is gone as far as the queue
// is concerned: it lets go of the queue as at its detach.
type dialogQueue struct {
	mu       sync.Mutex
	items    []*dialogItem // queued, not yet shown, oldest first
	app      uint64        // the attachment that holds the queue, 0 for none
	inFlight *dialogItem   // the request the app shows, nil for none
	running  bool          // a goroutine shows this machine's dialogs
	closed   bool          // close was called: nothing queues, shows or times out

	// The app's last poll (SetApp on, or Next), and the timer that
	// checks for staleness while it holds the queue: stopTimer stops
	// it, timerGen numbers it, so that one stopped too late does
	// nothing. now and afterFunc are the clock, time's unless a test's.
	lastPoll  time.Time
	stopTimer func() bool
	timerGen  uint64
	now       func() time.Time
	afterFunc func(time.Duration, func()) func() bool
}

// dialogAppStale is how long an app that holds the queue may go without
// polling for dialogs before the queue lets go of it: the menu-bar app
// polls every two seconds, so this is many polls missed (a Mac that
// sleeps stops the clock that measures it, with the app).
const dialogAppStale = 30 * time.Second

// dialogItem is one request's dialog, with the link's dialog that
// answers it.
type dialogItem struct {
	d   *urlDialog
	env string
	id  uint64
	url string
}

var _ daemon.Dialogs = (*dialogQueue)(nil)

func newDialogQueue() *dialogQueue {
	return &dialogQueue{now: time.Now, afterFunc: func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }}
}

// polled records a poll of the app that holds the queue, and has a timer
// check for staleness when none does. Under mu.
func (q *dialogQueue) polled() {
	q.lastPoll = q.now()
	if q.stopTimer == nil {
		q.arm(dialogAppStale)
	}
}

// arm starts the staleness timer, to fire after d. Under mu.
func (q *dialogQueue) arm(d time.Duration) {
	if q.closed {
		return
	}
	q.timerGen++
	gen := q.timerGen
	q.stopTimer = q.afterFunc(d, func() { q.checkStale(gen) })
}

// disarm stops the staleness timer. Under mu.
func (q *dialogQueue) disarm() {
	if q.stopTimer != nil {
		q.stopTimer()
		q.stopTimer = nil
	}
	q.timerGen++
}

// checkStale is timer gen's: when the app that holds the queue has not
// polled for dialogAppStale, the queue lets go of it, as at its detach
// (the request in flight with it denied, those queued back to this
// machine's dialog); else the timer is set again for when it would be.
func (q *dialogQueue) checkStale(gen uint64) {
	q.mu.Lock()
	if gen != q.timerGen || q.app == 0 || q.closed {
		q.mu.Unlock()
		return
	}
	q.stopTimer = nil
	if idle := q.now().Sub(q.lastPoll); idle < dialogAppStale {
		q.arm(dialogAppStale - idle)
		q.mu.Unlock()
		return
	}
	gone := q.release()
	q.mu.Unlock()
	if gone != nil {
		gone.d.denyGone(gone.id, broker.ByApp)
	}
}

// release ends the app's hold on the queue, and returns the request that
// was in flight with it. Under mu.
func (q *dialogQueue) release() *dialogItem {
	gone := q.inFlight
	q.inFlight, q.app = nil, 0
	q.disarm()
	// What the app had not taken goes back to this machine's dialog.
	q.kick()
	return gone
}

// push queues a dialog.
func (q *dialogQueue) push(it *dialogItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.items = append(q.items, it)
	q.kick()
}

// kick starts showing this machine's dialogs, when no app holds the
// queue and some wait. Under mu.
func (q *dialogQueue) kick() {
	if q.app != 0 || q.running || q.closed || len(q.items) == 0 {
		return
	}
	q.running = true
	go q.run()
}

// run shows this machine's dialogs, one at a time, until none waits or
// an app holds the queue.
func (q *dialogQueue) run() {
	for {
		q.mu.Lock()
		if q.app != 0 || q.closed || len(q.items) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		it := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()
		it.d.show(it.env, it.id, it.url)
	}
}

// close ends the queue, as the process's links stop: its staleness
// timer stops, what is queued is forgotten (those requests stay
// pending), and nothing pushed after shows or starts a goroutine. A
// dialog showing goes on to its end, and answers only if its link still
// serves (urlDialog.serving); the goroutine showing it then ends.
func (q *dialogQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.disarm()
	q.closed = true
	q.items = nil
}

// drop forgets the queued dialogs of d, whose link stopped: their
// requests stay pending.
func (q *dialogQueue) drop(d *urlDialog) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = slices.DeleteFunc(q.items, func(it *dialogItem) bool { return it.d == d })
}

// SetApp is daemon.Dialogs's.
func (q *dialogQueue) SetApp(n uint64, on bool) {
	q.mu.Lock()
	var gone *dialogItem
	switch {
	case on && q.app == n:
		q.polled()
	case on:
		// Another attachment held it: it is gone (its detach may come
		// later, and then changes nothing).
		gone, q.inFlight, q.app = q.inFlight, nil, n
		q.disarm()
		q.polled()
	case q.app == n && n != 0:
		gone = q.release()
	}
	q.mu.Unlock()
	if gone != nil {
		gone.d.denyGone(gone.id, broker.ByApp)
	}
}

// errNotHolder refuses dialogs to an attachment that does not hold the
// queue (it went away meanwhile).
var errNotHolder = daemonproto.NewError(daemonproto.KindBadRequest, "only the menu-bar app's attached connection may call this")

// Next is daemon.Dialogs's.
func (q *dialogQueue) Next(n uint64) (*daemonproto.DialogRef, error) {
	for {
		q.mu.Lock()
		if n == 0 || q.app != n {
			q.mu.Unlock()
			return nil, errNotHolder
		}
		q.polled()
		it, fresh := q.inFlight, false
		if it == nil {
			if len(q.items) == 0 {
				q.mu.Unlock()
				return nil, nil
			}
			it, fresh = q.items[0], true
			q.items = q.items[1:]
			q.inFlight = it
		}
		q.mu.Unlock()
		if it.d.pending(it.id) {
			// In flight from here on, in requests.json too: a daemon that
			// goes away now leaves it to be denied, never asked again.
			// Unmarked, it is not handed out: it goes back to the front
			// of the queue, for the next call to try again.
			if fresh && !it.d.mark(it.id, broker.ByApp, "it stays queued") {
				q.mu.Lock()
				if q.inFlight == it {
					q.inFlight = nil
					q.items = slices.Insert(q.items, 0, it)
					q.kick()
				}
				q.mu.Unlock()
				return nil, nil
			}
			return &daemonproto.DialogRef{Env: it.env, ID: it.id}, nil
		}
		// Answered meanwhile (by the app, the command line or the page),
		// or its link stopped: the next one.
		q.mu.Lock()
		if q.inFlight == it {
			q.inFlight = nil
		}
		q.mu.Unlock()
	}
}

// pending reports whether request id still waits, and its link serves.
func (d *urlDialog) pending(id uint64) bool {
	pending := false
	d.serving(func() {
		q, err := d.answers.Request(id)
		pending = err == nil && q.State == broker.StatePending
	})
	return pending
}

// mark marks request id in flight with by ("" for no one), and reports
// whether it did; when it did not, it logs so, and then.
func (d *urlDialog) mark(id uint64, by, then string) bool {
	var err error
	if !d.serving(func() { err = d.answers.MarkInFlight(id, by) }) {
		return false
	}
	if err != nil {
		d.logf("request #%d: cannot record who shows its dialog (%v): %s", id, err, then)
		return false
	}
	return true
}

// denyGone denies request id, whose dialog by showed and went away. A
// link that stopped denies nothing: the mark in requests.json has the
// next broker deny it.
func (d *urlDialog) denyGone(id uint64, by string) {
	var err error
	if !d.serving(func() { err = d.answers.DenyInFlight(id, by) }) {
		return
	}
	switch {
	case errors.Is(err, broker.ErrAlreadyAnswered):
	case err != nil:
		d.logf("request #%d: cannot deny it now that %s went away: %v", id, by, err)
	default:
		d.logf("request #%d: denied: %s", id, broker.InFlightReason(by))
	}
}
