package vz

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Adding never waits, however many wait in the queue.
func TestWorkQueue(t *testing.T) {
	q := newWorkQueue[int]()
	done := make(chan struct{})
	go func() {
		for i := range 10000 {
			q.put(i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("put waited")
	}
	for i := range 10000 {
		if v, ok := q.get(); !ok || v != i {
			t.Fatalf("got %d %v, want %d", v, ok, i)
		}
	}
	q.close()
	if _, ok := q.get(); ok {
		t.Error("a closed, empty queue gave an element")
	}
}

// Returns never overlap, however many workers finish at once.
func TestInflightSerial(t *testing.T) {
	s := newInflight()
	s.start()
	var in, overlaps atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				e, st := s.take()
				if st != takeOK || !s.begin(e) {
					t.Error("refused")
					return
				}
				s.finish(e, func() {
					if in.Add(1) != 1 {
						overlaps.Add(1)
					}
					time.Sleep(time.Microsecond)
					in.Add(-1)
				})
			}
		}()
	}
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Errorf("%d returns overlapped", overlaps.Load())
	}
	if took, returned, _, now, _ := s.counts(); took != 3200 || returned != 3200 || now != 0 {
		t.Errorf("took %d, returned %d, %d in flight", took, returned, now)
	}
}

// A reset waits for what is in flight, takes nothing more, and never has
// an element of the ended epoch begun or returned.
func TestInflightEnd(t *testing.T) {
	s := newInflight()
	if _, st := s.take(); st != takeNo {
		t.Error("took work before the driver was ready")
	}
	s.start()
	e1, _ := s.take()
	e2, _ := s.take()
	s.begin(e1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		s.finish(e1, func() {})
	}()
	t0 := time.Now()
	was, left := s.end(100*time.Millisecond, nil)
	if waited := time.Since(t0); was != 2 || left != 1 || waited < 90*time.Millisecond {
		t.Errorf("%d in flight, left %d after %v", was, left, waited)
	}
	if _, st := s.take(); st != takeNo {
		t.Error("took work after a reset")
	}
	if s.begin(e2) {
		t.Error("an element of an ended epoch was begun")
	}
	s.start()
	e3, st := s.take()
	if st != takeOK || e3 == e2 || !s.begin(e3) || !s.finish(e3, func() {}) {
		t.Errorf("after the restart: %v, epochs %d %d", st, e2, e3)
	}
	// Nothing in flight: no wait.
	t0 = time.Now()
	if _, left := s.end(time.Second, nil); left != 0 || time.Since(t0) > 100*time.Millisecond {
		t.Errorf("an idle reset: %d left after %v", left, time.Since(t0))
	}
}

// A reset waits only as long as it must.
func TestInflightEndEarly(t *testing.T) {
	s := newInflight()
	s.start()
	e, _ := s.take()
	s.begin(e)
	go func() { time.Sleep(10 * time.Millisecond); s.finish(e, func() {}) }()
	t0 := time.Now()
	if _, left := s.end(5*time.Second, nil); left != 0 || time.Since(t0) > time.Second {
		t.Errorf("%d left after %v", left, time.Since(t0))
	}
}

// A reset that gives up on the elements in flight still waits, past its
// bound, for those being served: once it is done nothing touches their
// memory. The one being served is not returned after; one not begun is
// never begun.
func TestInflightEndWaitsForBusy(t *testing.T) {
	s := newInflight()
	s.start()
	busy, _ := s.take()
	queued, _ := s.take()
	if !s.begin(busy) {
		t.Fatal("refused to begin")
	}
	var served atomic.Bool
	go func() {
		time.Sleep(150 * time.Millisecond) // the I/O, long
		served.Store(true)
		if s.finish(busy, func() { t.Error("returned after its epoch ended") }) {
			t.Error("finish said it returned")
		}
	}()
	var slow atomic.Int32
	t0 := time.Now()
	was, left := s.end(20*time.Millisecond, func(n int) {
		if n != 1 {
			t.Errorf("slow: %d busy", n)
		}
		slow.Add(1)
	})
	if !served.Load() {
		t.Errorf("the reset was done after %v while a request was still being served", time.Since(t0))
	}
	if was != 2 || left != 2 || slow.Load() == 0 {
		t.Errorf("%d in flight, %d left, slow told %d times", was, left, slow.Load())
	}
	if s.begin(queued) {
		t.Error("an element not begun before the reset was begun after")
	}
	if _, ret, dropped, _, _ := s.counts(); ret != 0 || dropped != 2 {
		t.Errorf("returned %d, dropped %d", ret, dropped)
	}
}

// A return that waits for something keeps neither the drain from taking
// nor a reset from going on: only other returns wait for it. The reset
// ends the epoch while that return is under way: the returned element is
// counted returned, not left out, and not taken off the next epoch's
// count.
func TestInflightReturnHoldsNothing(t *testing.T) {
	s := newInflight()
	s.start()
	e, _ := s.take()
	s.begin(e)
	release := make(chan struct{})
	returning := make(chan struct{})
	go s.finish(e, func() { close(returning); <-release })
	<-returning
	took := make(chan takeState, 1)
	go func() { _, st := s.take(); took <- st }()
	select {
	case st := <-took:
		if st != takeOK {
			t.Errorf("take: %v", st)
		}
	case <-time.After(time.Second):
		t.Fatal("take waited for a return")
	}
	// A reset gets on with its waiting, telling of the one still busy.
	told := make(chan int, 16)
	ended := make(chan struct{})
	var was, left int
	go func() {
		was, left = s.end(20*time.Millisecond, func(n int) { told <- n })
		close(ended)
	}()
	select {
	case <-told:
	case <-time.After(time.Second):
		t.Fatal("a reset was held up by a return")
	}
	select {
	case <-ended:
		t.Fatal("a reset was done while an element was being returned")
	default:
	}
	close(release)
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("the reset did not end once the return did")
	}
	// In flight: the one being returned and the one taken after it.
	if was != 2 || left != 1 {
		t.Errorf("end: %d in flight, %d left", was, left)
	}
	if _, returned, dropped, now, _ := s.counts(); returned != 1 || dropped != 0 || now != 0 {
		t.Errorf("returned %d, dropped %d, %d in flight", returned, dropped, now)
	}
}

// A pause takes no more work but holds it, waits for what is in flight,
// and resume takes work again.
func TestInflightPause(t *testing.T) {
	s := newInflight()
	s.start()
	e, _ := s.take()
	s.begin(e)
	go func() { time.Sleep(10 * time.Millisecond); s.finish(e, func() {}) }()
	if left := s.pause(time.Second); left != 0 {
		t.Errorf("pause: %d left", left)
	}
	if _, st := s.take(); st != takeHold {
		t.Errorf("take while paused: %v", st)
	}
	s.resume()
	if _, st := s.take(); st != takeOK {
		t.Errorf("take after resume: %v", st)
	}
	// A reset while paused leaves the device taking nothing.
	s.pause(0)
	s.end(time.Millisecond, nil)
	if _, st := s.take(); st != takeNo {
		t.Errorf("take after a reset while paused: %v", st)
	}
	// What a pause held and a reset let go of is dropped.
	_, _, before, _, _ := s.counts()
	s.drop(2)
	if _, _, after, _, _ := s.counts(); after != before+2 {
		t.Errorf("dropped %d -> %d", before, after)
	}
}
