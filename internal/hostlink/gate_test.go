package hostlink

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestExecGate(t *testing.T) {
	g := NewExecGate()
	if !g.enter() || g.InFlight() != 1 {
		t.Fatal("an open gate refused")
	}
	if g.Drain(true) {
		t.Fatal("drained with a request in")
	}
	// A drain that finds a request in still closes the gate: a steady
	// stream of new requests cannot hold it off.
	if g.enter() {
		t.Fatal("admitted while a drain waits")
	}
	if g.InFlight() != 1 || g.Drain(true) {
		t.Fatalf("in flight %d after a refusal, or drained with one in", g.InFlight())
	}
	// One that does not hold opens the gate again.
	if g.Drain(false) || !g.enter() {
		t.Fatal("a drain that does not hold left the gate closed")
	}
	g.leave()
	g.leave()
	if !g.Drain(false) || g.InFlight() != 0 {
		t.Fatal("no drain with none in")
	}
	if g.enter() {
		t.Fatal("admitted after a drain")
	}
	if !g.Drain(true) {
		t.Fatal("a second drain")
	}
	var nilGate *ExecGate
	if !nilGate.enter() || !nilGate.Drain(false) || nilGate.InFlight() != 0 {
		t.Fatal("a nil gate")
	}
	nilGate.leave()
}

// Under contention, no request is ever admitted after a drain succeeded.
func TestExecGateConcurrent(t *testing.T) {
	for range 50 {
		g := NewExecGate()
		var drained atomic.Bool
		var late atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 200 {
					if g.enter() {
						if drained.Load() {
							late.Add(1)
						}
						g.leave()
					}
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !g.Drain(true) {
			}
			drained.Store(true)
		}()
		wg.Wait()
		if late.Load() != 0 {
			t.Fatalf("%d requests admitted after the drain", late.Load())
		}
		if g.enter() {
			t.Fatal("admitted after the drain")
		}
	}
}

// Once its gate is drained, a link refuses host exec, saying why.
func TestHostExecRefusedOnceDrained(t *testing.T) {
	g := NewExecGate()
	if !g.Drain(true) {
		t.Fatal("drain")
	}
	h := &Host{cfg: Config{HostExec: &HostExec{}, Gate: g}}
	if err := h.hostExec(1); err == nil || !strings.Contains(err.Error(), "being replaced") {
		t.Fatalf("host exec once drained: %v", err)
	}
	if g.InFlight() != 0 {
		t.Errorf("in flight %d", g.InFlight())
	}
	// While a drain waits for a running command, a new one is refused too.
	w := NewExecGate()
	if !w.enter() || w.Drain(true) {
		t.Fatal("a drain with a request in")
	}
	if w.enter() {
		t.Fatal("admitted while a drain holds")
	}
	hw := &Host{cfg: Config{HostExec: &HostExec{}, Gate: w}}
	if err := hw.hostExec(1); err == nil || !strings.Contains(err.Error(), "being replaced") {
		t.Fatalf("host exec while a drain waits: %v", err)
	}
	if w.InFlight() != 1 {
		t.Errorf("in flight %d, want the one running", w.InFlight())
	}
	w.leave()
	if !w.Drain(true) {
		t.Error("no drain once the running one ended")
	}
	// A refusal for another reason leaves the gate as it was.
	g2 := NewExecGate()
	h2 := &Host{cfg: Config{HostExec: &HostExec{}, Gate: g2}}
	h2.waitExecs()
	if err := h2.hostExec(1); err == nil || g2.InFlight() != 0 || !g2.Drain(true) {
		t.Fatalf("closing link: %v, in flight %d", err, g2.InFlight())
	}
}
