package vm

import (
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agent"
	"github.com/bfreis/caboose/internal/agentproto"
)

type volGuest struct {
	mu      sync.Mutex
	ops     []agentproto.VolumeOp
	release chan struct{}
}

func (g *volGuest) Boot(agentproto.BootSpec) error { return nil }
func (g *volGuest) Shutdown()                      {}

func (g *volGuest) Volume(op agentproto.VolumeOp) agentproto.VolumeInfo {
	g.mu.Lock()
	g.ops = append(g.ops, op)
	g.mu.Unlock()
	if g.release != nil {
		<-g.release
	}
	return agentproto.VolumeInfo{Op: op.Op, Mounted: true, DiskBytes: op.Size}
}

type plainGuest struct{}

func (plainGuest) Boot(agentproto.BootSpec) error { return nil }
func (plainGuest) Shutdown()                      {}

type stillClock struct{}

func (stillClock) Now() time.Time           { return time.Now() }
func (stillClock) Step(time.Time) error     { return nil }
func (stillClock) Slew(time.Duration) error { return nil }

func controlTo(t *testing.T, g agent.Guest) *Control {
	t.Helper()
	h, a := net.Pipe()
	srv := &agent.ControlServer{Guest: g, Clock: stillClock{}}
	go func() { _ = srv.ServeConn(a) }()
	c, err := NewControl(h, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// A volume step goes to the guest and its answer comes back; a guest that
// resizes nothing says so; a second step while one runs is refused.
func TestControlVolume(t *testing.T) {
	g := &volGuest{}
	c := controlTo(t, g)
	info, err := c.Volume(agentproto.VolumeOp{Op: agentproto.VolumeGrow, Name: "cache", Size: 8 << 30}, 5*time.Second)
	if err != nil || info.Op != "grow" || info.DiskBytes != 8<<30 || !info.Mounted {
		t.Fatalf("%+v, %v", info, err)
	}
	if len(g.ops) != 1 || g.ops[0].Name != "cache" {
		t.Errorf("ops %+v", g.ops)
	}

	info, err = controlTo(t, plainGuest{}).Volume(agentproto.VolumeOp{Op: agentproto.VolumeUsage, Name: "cache"}, 5*time.Second)
	if err != nil || !strings.Contains(info.Error, "resizes no volumes") {
		t.Errorf("a guest without volumes: %+v, %v", info, err)
	}

	g = &volGuest{release: make(chan struct{})}
	srv := &agent.ControlServer{Guest: g, Clock: stillClock{}}
	conn := func() *Control {
		h, a := net.Pipe()
		go func() { _ = srv.ServeConn(a) }()
		c, err := NewControl(h, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	first, second := conn(), conn()
	done := make(chan error, 1)
	go func() {
		_, err := first.Volume(agentproto.VolumeOp{Op: agentproto.VolumeShrinkFS, Name: "cache", Size: 4 << 30}, 5*time.Second)
		done <- err
	}()
	for {
		g.mu.Lock()
		n := len(g.ops)
		g.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	info, err = second.Volume(agentproto.VolumeOp{Op: agentproto.VolumeUsage, Name: "cache"}, 5*time.Second)
	if err != nil || !strings.Contains(info.Error, "another step") {
		t.Errorf("a second step: %+v, %v", info, err)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Errorf("the first step: %v", err)
	}
}

// An agent that never answers (one from before volume resizes) is a
// timeout the caller can tell.
func TestControlVolumeNoAnswer(t *testing.T) {
	g := &volGuest{release: make(chan struct{})}
	defer close(g.release)
	c := controlTo(t, g)
	_, err := c.Volume(agentproto.VolumeOp{Op: agentproto.VolumeUsage, Name: "cache"}, 100*time.Millisecond)
	if !errors.Is(err, ErrNoVolumeAnswer) {
		t.Errorf("%v", err)
	}
}
