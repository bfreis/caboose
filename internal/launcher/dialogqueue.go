package launcher

import (
	"slices"
	"sync"

	"github.com/bfreis/caboose/internal/broker"
)

// dialogQueue is the URL dialogs of every link in this process (the
// daemon's, or a helper's one link), shown one at a time by this
// machine's own dialog (urlDialog.show), in the order their requests were
// filed. The dialog marks its request in flight in requests.json first;
// the request is denied, never asked again, if the dialog goes away
// before it is answered.
type dialogQueue struct {
	mu      sync.Mutex
	items   []*dialogItem // queued, not yet shown, oldest first
	running bool          // a goroutine shows this machine's dialogs
	closed  bool          // close was called: nothing queues or shows
}

// dialogItem is one request's dialog, with the link's dialog that
// answers it.
type dialogItem struct {
	d   *urlDialog
	env string
	id  uint64
	url string
}

func newDialogQueue() *dialogQueue { return &dialogQueue{} }

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

// kick starts showing this machine's dialogs, when some wait. Under mu.
func (q *dialogQueue) kick() {
	if q.running || q.closed || len(q.items) == 0 {
		return
	}
	q.running = true
	go q.run()
}

// run shows this machine's dialogs, one at a time, until none waits.
func (q *dialogQueue) run() {
	for {
		q.mu.Lock()
		if q.closed || len(q.items) == 0 {
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

// close ends the queue, as the process's links stop: what is queued is
// forgotten (those requests stay pending), and nothing pushed after
// shows or starts a goroutine. A dialog showing goes on to its end, and
// answers only if its link still serves (urlDialog.serving); the
// goroutine showing it then ends.
func (q *dialogQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
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
