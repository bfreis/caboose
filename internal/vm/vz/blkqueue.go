package vz

import (
	"sync"
	"time"
)

// workQueue hands elements from the device's queue to the workers. Adding
// never blocks: the device's queue must never wait for a worker.
type workQueue[T any] struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []T
	closed bool
}

func newWorkQueue[T any]() *workQueue[T] {
	q := &workQueue[T]{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *workQueue[T]) put(v T) {
	q.mu.Lock()
	q.items = append(q.items, v)
	q.mu.Unlock()
	q.cond.Signal()
}

// get waits for an element; false once the queue is closed and empty.
func (q *workQueue[T]) get() (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	var zero T
	if len(q.items) == 0 {
		return zero, false
	}
	v := q.items[0]
	q.items[0] = zero
	q.items = q.items[1:]
	return v, true
}

func (q *workQueue[T]) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// inflight follows a device's elements from the queue that gave them to
// their return, and makes the returns one at a time: the framework loses
// completions returned at once from several threads. Every element
// belongs to an epoch, which a reset or a stop ends: once end has
// returned, no element of the epoch it ended is ever returned, as the
// framework raises an exception (which ends the process) on an element it
// no longer knows. A return already under way when the epoch ends is let
// finish, and end waits for it.
//
// An element is busy from the start of its serving (begin) to the end of
// its return: only then does anything touch the guest's memory it names,
// or the file for it. A reset or a stop is done only once nothing is
// busy, and an element not yet begun then is never begun.
type inflight struct {
	mu   sync.Mutex
	idle *sync.Cond
	// ret is held across a return, which makes them one at a time.
	// Nothing that waits for elements (end, pause) ever takes it, so a
	// return that waits for something of the device's never holds up mu.
	ret    sync.Mutex
	epoch  uint64
	open   bool
	paused bool
	n      int // this epoch's, taken and not yet handed back
	busy   int
	// returning counts the busy elements whose return is under way.
	returning int
	max       int

	took, returned, dropped int64
}

func newInflight() *inflight {
	s := &inflight{}
	s.idle = sync.NewCond(&s.mu)
	return s
}

// What take says of an element.
type takeState int

const (
	// takeOK: the device serves it, in the epoch take says.
	takeOK takeState = iota
	// takeHold: the device is paused; the element waits for resume.
	takeHold
	// takeNo: the device takes no work (before the driver is ready, or
	// after a reset or a stop).
	takeNo
)

// take counts one more element in flight and says its epoch, or why the
// device does not take it now.
func (s *inflight) take() (uint64, takeState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.open:
		return 0, takeNo
	case s.paused:
		return 0, takeHold
	}
	s.n++
	s.max = max(s.max, s.n)
	s.took++
	return s.epoch, takeOK
}

// begin makes an element of epoch busy, when its epoch has not ended: it
// may then be served and must be finished. An element of an ended epoch
// is counted dropped and must not be touched.
func (s *inflight) begin(epoch uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch != s.epoch {
		s.dropped++
		return false
	}
	s.busy++
	return true
}

// finish ends a busy element of epoch: ret hands it back, only when its
// epoch has not ended, and never while another is handed back. It says
// whether ret ran.
func (s *inflight) finish(epoch uint64, ret func()) bool {
	s.ret.Lock()
	defer s.ret.Unlock()
	s.mu.Lock()
	cur := epoch == s.epoch
	if cur {
		s.returning++
	}
	s.mu.Unlock()
	// An end that comes now waits for this element, still busy.
	if cur {
		ret()
	}
	s.mu.Lock()
	if cur {
		s.returning--
	}
	s.settleOne(epoch, cur)
	s.mu.Unlock()
	return cur
}

// settleOne counts a busy element of epoch done, returned or not; s.mu
// held. An element whose return began before an end ended its epoch is
// counted returned, but no longer in flight: end has set n for the next
// epoch.
func (s *inflight) settleOne(epoch uint64, returned bool) {
	s.busy--
	if returned {
		s.returned++
		if epoch == s.epoch {
			s.n--
		}
	} else {
		s.dropped++
	}
	if s.n == 0 || s.busy == 0 {
		s.idle.Broadcast()
	}
}

// drop counts n elements the device let go of without serving them.
func (s *inflight) drop(n int) {
	s.mu.Lock()
	s.dropped += int64(n)
	s.mu.Unlock()
}

// start opens a new epoch: the device takes work.
func (s *inflight) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		s.epoch++
		s.n = 0
		s.open, s.paused = true, false
	}
}

// end stops taking work and waits at most wait for what is in flight to
// be handed back. Then it ends the epoch, so that an element not yet
// begun is never begun, and waits for those still busy, however long
// that takes: their I/O and their return are bounded, and once end
// returns nothing touches the guest's memory or the file for them. slow
// is told every wait of how many are still busy. end says how many were
// in flight when it began (was), and how many it left out (left): those
// neither handed back nor being handed back when it ended the epoch,
// which are never returned. An element whose return was under way is
// counted returned when that return ends.
func (s *inflight) end(wait time.Duration, slow func(busy int)) (was, left int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open, s.paused = false, false
	was = s.n
	s.waitUntil(func() bool { return s.n == 0 }, wait)
	left = s.n - s.returning
	s.epoch++
	s.n = 0
	for !s.waitUntil(func() bool { return s.busy == 0 }, wait) {
		if slow != nil {
			busy := s.busy
			s.mu.Unlock()
			slow(busy)
			s.mu.Lock()
		}
	}
	return was, left
}

// pause stops taking work, an element that comes meanwhile held for
// resume, and waits at most wait for what is in flight to be handed
// back; it says how many still are.
func (s *inflight) pause(wait time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open {
		s.paused = true
	}
	s.waitUntil(func() bool { return s.n == 0 }, wait)
	return s.n
}

// resume takes work again after a pause.
func (s *inflight) resume() {
	s.mu.Lock()
	s.paused = false
	s.mu.Unlock()
}

// waitUntil waits, s.mu held, at most wait for ok, and says whether it
// came.
func (s *inflight) waitUntil(ok func() bool, wait time.Duration) bool {
	if ok() {
		return true
	}
	deadline := time.Now().Add(wait)
	t := time.AfterFunc(wait, func() {
		s.mu.Lock()
		s.idle.Broadcast()
		s.mu.Unlock()
	})
	defer t.Stop()
	for !ok() {
		if !time.Now().Before(deadline) {
			return false
		}
		s.idle.Wait()
	}
	return true
}

// counts is what s has seen.
func (s *inflight) counts() (took, returned, dropped int64, now, most int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.took, s.returned, s.dropped, s.n, s.max
}
