package agent

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	steps []time.Time
	slews []time.Duration
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) Step(to time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps = append(c.steps, to)
	c.now = to
	return nil
}

func (c *fakeClock) Slew(d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slews = append(c.slews, d)
	return nil
}

func (c *fakeClock) done() ([]time.Time, []time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.steps, c.slews
}

// The rule spike 5 called for: forward always steps, a little ahead
// slews, a lot ahead steps, and a few milliseconds are left alone.
func TestAdjustClock(t *testing.T) {
	base := time.Unix(1_790_000_000, 0)
	for _, tc := range []struct {
		name       string
		host       time.Duration // the host's time, from the guest's
		step, slew bool
		slewBy     time.Duration
	}{
		{"in step", 5 * time.Millisecond, false, false, 0},
		{"a little ahead, within tolerance", -5 * time.Millisecond, false, false, 0},
		{"behind after a sleep", 6300 * time.Second, true, false, 0},
		{"behind a little", 40 * time.Millisecond, true, false, 0},
		{"ahead a little", -300 * time.Millisecond, false, true, -300 * time.Millisecond},
		{"ahead a lot", -2 * time.Second, true, false, 0},
	} {
		c := &fakeClock{now: base}
		if _, err := adjustClock(c, base.Add(tc.host)); err != nil {
			t.Fatal(err)
		}
		steps, slews := c.done()
		if (len(steps) == 1) != tc.step || (len(slews) == 1) != tc.slew {
			t.Errorf("%s: steps %v, slews %v", tc.name, steps, slews)
		}
		if tc.step && !steps[0].Equal(base.Add(tc.host)) {
			t.Errorf("%s: stepped to %v", tc.name, steps[0])
		}
		if tc.slew && slews[0] != tc.slewBy {
			t.Errorf("%s: slewed by %v", tc.name, slews[0])
		}
	}
}

type fakeGuest struct {
	mu       sync.Mutex
	boots    []agentproto.BootSpec
	err      error
	release  chan struct{}
	shutdown chan struct{}
}

func (g *fakeGuest) Boot(s agentproto.BootSpec) error {
	g.mu.Lock()
	g.boots = append(g.boots, s)
	g.mu.Unlock()
	if g.release != nil {
		<-g.release
	}
	return g.err
}

func (g *fakeGuest) Shutdown() { close(g.shutdown) }

func (g *fakeGuest) bootCount() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.boots) }

// controlConn connects a host session to c.
func controlConn(t *testing.T, c *ControlServer) (*agentproto.Session, chan error) {
	t.Helper()
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- c.ServeConn(rwc{ar, aw}) }()
	host := agentproto.NewSession(hr, hw, true)
	t.Cleanup(func() { host.Close() })
	return host, done
}

type rwc struct {
	io.Reader
	io.WriteCloser
}

func send(t *testing.T, s *agentproto.Session, m agentproto.Message) {
	t.Helper()
	if err := s.Send(m); err != nil {
		t.Fatal(err)
	}
}

func recv(t *testing.T, s *agentproto.Session) agentproto.Message {
	t.Helper()
	select {
	case b, ok := <-s.Control():
		if !ok {
			t.Fatalf("session ended: %v", s.Err())
		}
		m, err := agentproto.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
	return agentproto.Message{}
}

func hello(t *testing.T, s *agentproto.Session) {
	t.Helper()
	send(t, s, agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version})
	if m := recv(t, s); m.Type != agentproto.TypeHello || m.Version != agentproto.Version {
		t.Fatalf("hello answered with %+v", m)
	}
}

// A launch: hello, the clock, the boot; ready once the guest is. The clock
// is set while the boot is still going.
func TestControlBoot(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_790_000_000, 0)}
	g := &fakeGuest{release: make(chan struct{}), shutdown: make(chan struct{})}
	c := &ControlServer{Guest: g, Clock: clock}
	host, done := controlConn(t, c)
	hello(t, host)
	spec := agentproto.BootSpec{Hostname: "caboose", Env: []string{"TZ=UTC"},
		Mounts: []agentproto.GuestMount{{Tag: "work", Target: "/work"}}}
	send(t, host, agentproto.Message{Type: agentproto.TypeBoot, Boot: &spec})
	later := clock.Now().Add(time.Hour)
	send(t, host, agentproto.Message{Type: agentproto.TypeClock, Time: later.UnixNano()})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if steps, _ := clock.done(); len(steps) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the clock was not set during the boot")
		}
	}
	close(g.release)
	if m := recv(t, host); m.Type != agentproto.TypeReady || m.Error != "" {
		t.Fatalf("got %+v", m)
	}
	if g.boots[0].Hostname != "caboose" || g.boots[0].Mounts[0].Target != "/work" {
		t.Errorf("booted %+v", g.boots[0])
	}

	// A later launch connects again: ready at once, no second boot.
	again, _ := controlConn(t, c)
	hello(t, again)
	send(t, again, agentproto.Message{Type: agentproto.TypeBoot, Boot: &spec})
	if m := recv(t, again); m.Type != agentproto.TypeReady || g.bootCount() != 1 {
		t.Fatalf("second boot: %+v, %d boots", m, g.bootCount())
	}

	send(t, host, agentproto.Message{Type: agentproto.TypeShutdown})
	select {
	case <-g.shutdown:
	case <-time.After(5 * time.Second):
		t.Fatal("no shutdown")
	}
	if err := <-done; err != nil {
		t.Errorf("served: %v", err)
	}
}

func TestControlBootFails(t *testing.T) {
	g := &fakeGuest{err: errors.New("no root disk"), shutdown: make(chan struct{})}
	host, _ := controlConn(t, &ControlServer{Guest: g, Clock: &fakeClock{}})
	hello(t, host)
	send(t, host, agentproto.Message{Type: agentproto.TypeBoot, Boot: &agentproto.BootSpec{}})
	if m := recv(t, host); m.Type != agentproto.TypeReady || m.Error != "no root disk" {
		t.Fatalf("got %+v", m)
	}
}

// Nothing is done for a host that has not said which version it speaks.
func TestControlNeedsHello(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_790_000_000, 0)}
	host, done := controlConn(t, &ControlServer{Guest: &fakeGuest{}, Clock: clock})
	send(t, host, agentproto.Message{Type: agentproto.TypeClock, Time: clock.Now().Add(time.Hour).UnixNano()})
	if err := <-done; err == nil {
		t.Error("served a clock before a hello")
	}
	if steps, _ := clock.done(); len(steps) != 0 {
		t.Errorf("stepped: %v", steps)
	}

	host, done = controlConn(t, &ControlServer{Guest: &fakeGuest{}, Clock: clock})
	send(t, host, agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version + 1})
	if err := <-done; err == nil {
		t.Error("served another version")
	}
}
