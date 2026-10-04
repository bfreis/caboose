package vm

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agent"
	"github.com/bfreis/caboose/internal/agentproto"
)

// guest is a Guest whose boot fails when told to.
type guest struct {
	mu    sync.Mutex
	boots []agentproto.BootSpec
	fail  string
	down  chan struct{}
}

func (g *guest) Boot(s agentproto.BootSpec) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.boots = append(g.boots, s)
	if g.fail != "" {
		return errors.New(g.fail)
	}
	return nil
}

func (g *guest) Shutdown() { close(g.down) }

// clock is a guest clock stuck at the epoch, so every clock message the
// host sends steps it; Steps are what it was set to.
type clock struct{ steps chan time.Time }

func (c *clock) Now() time.Time                { return time.Unix(0, 0) }
func (c *clock) Step(t time.Time) error        { c.steps <- t; return nil }
func (c *clock) Slew(d time.Duration) error    { return nil }
func newClock() *clock                         { return &clock{steps: make(chan time.Time, 16)} }
func (c *clock) next(t *testing.T) time.Time   { return recv(t, c.steps) }
func (c *clock) none(t *testing.T, why string) { noRecv(t, c.steps, why) }

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived")
		panic("unreachable")
	}
}

func noRecv[T any](t *testing.T, ch <-chan T, why string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("%s: got %v", why, v)
	case <-time.After(50 * time.Millisecond):
	}
}

// pair is a Control connected to a ControlServer over a pipe.
func pair(t *testing.T, g *guest, c *clock) (*Control, net.Conn) {
	t.Helper()
	host, agentEnd := net.Pipe()
	srv := &agent.ControlServer{Guest: g, Clock: c}
	go func() { _ = srv.ServeConn(agentEnd) }()
	ctl, err := NewControl(host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctl.Close() })
	return ctl, agentEnd
}

func TestControlBootReady(t *testing.T) {
	g, c := &guest{down: make(chan struct{})}, newClock()
	ctl, _ := pair(t, g, c)
	at := time.Unix(1_800_000_000, 0)
	if err := ctl.Clock(at); err != nil {
		t.Fatal(err)
	}
	if got := c.next(t); !got.Equal(at) {
		t.Fatalf("clock set to %v, want %v", got, at)
	}
	spec := agentproto.BootSpec{Hostname: "box", User: "0:0", Cmd: []string{"/e"}, Ready: "/r"}
	if err := ctl.Boot(spec); err != nil {
		t.Fatal(err)
	}
	if err := ctl.Ready(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g.boots, []agentproto.BootSpec{spec}) {
		t.Fatalf("booted %+v", g.boots)
	}
	if err := ctl.Shutdown(); err != nil {
		t.Fatal(err)
	}
	recv(t, g.down)
}

// A failed boot comes back as its error, not as a timeout.
func TestControlBootFails(t *testing.T) {
	g := &guest{down: make(chan struct{}), fail: "no entrypoint"}
	ctl, _ := pair(t, g, newClock())
	if err := ctl.Boot(agentproto.BootSpec{User: "0", Cmd: []string{"/e"}, Ready: "/r"}); err != nil {
		t.Fatal(err)
	}
	if err := ctl.Ready(5 * time.Second); err == nil || !strings.Contains(err.Error(), "no entrypoint") {
		t.Fatalf("err = %v", err)
	}
}

// An agent of another version is refused at the hello, saying why.
func TestControlVersion(t *testing.T) {
	host, agentEnd := net.Pipe()
	go func() {
		s := agentproto.NewSession(agentEnd, agentEnd, false)
		<-s.Control()
		_ = s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version + 1})
		<-s.Done()
	}()
	_, err := NewControl(host, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "another caboose") {
		t.Fatalf("err = %v", err)
	}
}

// fakeNow is a host clock the test moves: wall and monotonic apart, as a
// sleep moves them.
type fakeNow struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func (f *fakeNow) now() (time.Time, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wall, f.mono
}

func (f *fakeNow) advance(wall, mono time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wall = f.wall.Add(wall)
	f.mono += mono
}

// The keeper sends the clock at once; again after a sleep (wall moves,
// monotonic does not) and after an hour; not on an ordinary tick; and on a
// new connection when the old one ends.
func TestClockKeeper(t *testing.T) {
	c := newClock()
	now := &fakeNow{wall: time.Unix(1_800_000_000, 0)}
	tick := make(chan time.Time)
	conns := make(chan net.Conn, 4)
	k := &ClockKeeper{
		Dial: func() (*Control, error) {
			host, agentEnd := net.Pipe()
			conns <- agentEnd
			srv := &agent.ControlServer{Guest: &guest{down: make(chan struct{})}, Clock: c}
			go func() { _ = srv.ServeConn(agentEnd) }()
			return NewControl(host, 5*time.Second)
		},
		Now:  now.now,
		Tick: tick,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { k.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	if got := c.next(t); !got.Equal(now.wall) {
		t.Fatalf("first clock %v", got)
	}
	now.advance(10*time.Second, 10*time.Second)
	tick <- time.Time{}
	c.none(t, "an ordinary tick")

	now.advance(2*time.Hour, 10*time.Second) // asleep
	tick <- time.Time{}
	if got := c.next(t); !got.Equal(now.wall) {
		t.Fatalf("after the sleep %v, want %v", got, now.wall)
	}

	for i := 0; i < 360; i++ {
		now.advance(10*time.Second, 10*time.Second)
		tick <- time.Time{}
	}
	c.next(t) // the hourly one
	c.none(t, "only one an hour")

	first := recv(t, conns)
	first.Close()
	// The keeper redials at a tick after it sees the end, which the tick
	// racing that may not be.
	for redialed := false; !redialed; {
		select {
		case tick <- time.Time{}:
		case <-conns:
			redialed = true
		case <-time.After(5 * time.Second):
			t.Fatal("no redial")
		}
	}
	c.next(t)
}

func TestStateRoundTrip(t *testing.T) {
	d := Dir(t.TempDir() + "/vm/box")
	if _, err := d.ReadState(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no state: err = %v", err)
	}
	st := State{Image: "sha256:x", Labels: map[string]string{"a": "b"}, Mounts: []Mount{{"/h", "/c"}},
		Boot: agentproto.BootSpec{User: "0:0"}, Machine: Machine{CPUs: 2, Network: "nat"}}
	if err := d.WriteState(st); err != nil {
		t.Fatal(err)
	}
	got, err := d.ReadState()
	if err != nil || !reflect.DeepEqual(got, st) {
		t.Fatalf("got %+v, %v", got, err)
	}
	fi, err := os.Stat(string(d))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, %v", fi.Mode(), err)
	}
}

// The command to run is in the helper's environment, not its arguments.
func TestExecCommandCarriesTheJob(t *testing.T) {
	req := agentproto.ExecRequest{Argv: []string{"tmux", "ls"}, Env: []string{"A=1"}, TTY: true}
	cmd := ExecCommand("/bin/caboose", "/d/vm.sock", req)
	if !reflect.DeepEqual(cmd.Args, []string{"/bin/caboose", ExecHelper}) {
		t.Fatalf("args %q", cmd.Args)
	}
	var job execJob
	for _, e := range cmd.Env {
		if v, ok := strings.CutPrefix(e, execEnv+"="); ok {
			if err := json.Unmarshal([]byte(v), &job); err != nil {
				t.Fatal(err)
			}
		}
	}
	if job.Socket != "/d/vm.sock" || !reflect.DeepEqual(job.Req, req) {
		t.Fatalf("job %+v", job)
	}
}
