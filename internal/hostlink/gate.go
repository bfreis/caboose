package hostlink

import "sync"

// ExecGate admits host-exec requests, counting each from its admission to
// the end of its command, until a Drain that finds none in closes it for
// good. It is what lets the daemon exit only once no host command runs,
// with none admitted between finding that and exiting. A Drain that holds
// keeps new requests out while the running ones end, so that a steady
// stream of them cannot hold the exit off. A nil gate admits everything.
type ExecGate struct {
	mu     sync.Mutex
	n      int
	closed bool
}

// NewExecGate is an open gate.
func NewExecGate() *ExecGate { return &ExecGate{} }

// enter admits one request, unless the gate is closed.
func (g *ExecGate) enter() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.n++
	return true
}

// leave ends a request enter admitted.
func (g *ExecGate) leave() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n--
}

// Drain closes the gate for good and reports true when no request is in.
// When one is, it reports false, and leaves the gate closed to new
// requests if hold, or open if not (reopening one an earlier Drain held).
func (g *ExecGate) Drain(hold bool) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n == 0 {
		g.closed = true
		return true
	}
	g.closed = hold
	return false
}

// InFlight is how many requests are in.
func (g *ExecGate) InFlight() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}
