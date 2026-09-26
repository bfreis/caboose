package launcher

import (
	"fmt"
	"sync"
	"time"
)

// spinnerFrames turn, one a tick, in front of what is going on.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinner is a line on a terminal that says what is going on while
// something takes its time -- a check asking docker, a container, a
// remote -- so that a command waiting on it does not look stuck. Output
// printed while it turns goes through above, which keeps it the last line.
type spinner struct {
	u     *ui
	mu    sync.Mutex
	what  string // what is going on; "" draws no line
	frame int
	stop  chan struct{}
	done  chan struct{}
}

// startSpinner turns on u, which must be a terminal, saying what.
func startSpinner(u *ui, what string) *spinner {
	s := &spinner{u: u, what: what, stop: make(chan struct{}), done: make(chan struct{})}
	s.mu.Lock()
	s.draw()
	s.mu.Unlock()
	go s.spin()
	return s
}

func (s *spinner) spin() {
	defer close(s.done)
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			s.frame++
			s.draw()
			s.mu.Unlock()
		}
	}
}

// draw writes the line over itself; the cursor stays on it.
func (s *spinner) draw() {
	if s.what == "" {
		return
	}
	f := spinnerFrames[s.frame%len(spinnerFrames)]
	fmt.Fprintf(s.u.out, "\r\x1b[K  %s %s", s.u.paint(cyan, f), s.u.paint(dim, s.what+"…"))
}

// clear takes the line off, leaving the cursor where it began.
func (s *spinner) clear() {
	if s.what != "" {
		fmt.Fprint(s.u.out, "\r\x1b[K")
	}
}

// set says what is going on now.
func (s *spinner) set(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
	s.what = what
	s.draw()
}

// above runs print, which writes whole lines, above the spinner's line.
func (s *spinner) above(print func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
	print()
	s.draw()
}

// end stops the spinner and takes its line off.
func (s *spinner) end() {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
	s.what = ""
}
