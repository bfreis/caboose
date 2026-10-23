package macui

import (
	"sync"
	"sync/atomic"

	"github.com/bfreis/caboose/internal/menubar"
)

// registry holds the functions a C callback runs, by the number it is
// handed as its context: a Go function cannot cross into C, a number can.
// Each is taken once.
type registry struct {
	mu   sync.Mutex
	next uintptr
	fns  map[uintptr]func()
}

// put stores fn and returns its number, never 0.
func (r *registry) put(fn func()) uintptr {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fns == nil {
		r.fns = make(map[uintptr]func())
	}
	r.next++
	if r.next == 0 {
		r.next++
	}
	r.fns[r.next] = fn
	return r.next
}

// take removes and returns the function numbered n, or nil.
func (r *registry) take(n uintptr) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn := r.fns[n]
	delete(r.fns, n)
	return fn
}

// sink is where the glue posts events: the Events Run was given, or
// nothing before Run and after it returns.
type sink struct {
	ev atomic.Pointer[menubar.Events]
}

func (s *sink) set(ev menubar.Events) {
	if ev == nil {
		s.ev.Store(nil)
		return
	}
	s.ev.Store(&ev)
}

// get is the Events to post to, or nil.
func (s *sink) get() menubar.Events {
	if p := s.ev.Load(); p != nil {
		return *p
	}
	return nil
}

// selected posts a menu item's selection; an item with no ID posts
// nothing.
func (s *sink) selected(id string) {
	if ev := s.get(); ev != nil && id != "" {
		ev.Selected(id)
	}
}

func (s *sink) menuOpened() {
	if ev := s.get(); ev != nil {
		ev.MenuOpened()
	}
}

func (s *sink) menuClosed() {
	if ev := s.get(); ev != nil {
		ev.MenuClosed()
	}
}

// notificationClicked posts a click on a notification; one with no
// identifier posts nothing.
func (s *sink) notificationClicked(id string) {
	if ev := s.get(); ev != nil && id != "" {
		ev.NotificationClicked(id)
	}
}

// notificationAuth posts the answer to the authorization request.
func (s *sink) notificationAuth(granted bool) {
	if ev := s.get(); ev != nil {
		ev.NotificationAuth(granted)
	}
}
