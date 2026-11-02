package agent

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

// Guest is the machine the control port runs: the init's, or a test's.
type Guest interface {
	// Boot makes the sandbox from spec, and returns once it is ready.
	Boot(agentproto.BootSpec) error
	// Shutdown stops the sandbox, then the machine.
	Shutdown()
}

// VolumeGuest is a Guest whose volumes can be resized while it runs: the
// sandbox's (machine). Volume does one step of a resize and answers it.
type VolumeGuest interface {
	Volume(agentproto.VolumeOp) agentproto.VolumeInfo
}

// Clock is the guest's wall clock.
type Clock interface {
	Now() time.Time
	// Step sets it.
	Step(to time.Time) error
	// Slew moves it by d gradually, never backwards.
	Slew(d time.Duration) error
}

// ControlServer serves the control port (agentproto's control.go).
type ControlServer struct {
	Guest Guest
	Clock Clock
	// Logf, when set, is told what the server did.
	Logf func(format string, args ...any)

	once    sync.Once
	bootErr error
	// volume makes a volume's steps one at a time, whichever connection
	// asks.
	volume sync.Mutex
}

// The clock is left alone within clockTolerance of the host's. Behind by
// more, it steps forward: a sleep of the host's froze it, and a file's
// time never goes back that way. Ahead, it slews back up to maxSlew, so it
// never runs backwards under a build; ahead by more is a clock gone wrong,
// and steps. (Asleep, a guest loses exactly the time slept; awake, it
// drifts only slightly.)
const (
	clockTolerance = 10 * time.Millisecond
	maxSlew        = 500 * time.Millisecond
)

// adjustClock brings c to host's time, and says what it did.
func adjustClock(c Clock, host time.Time) (string, error) {
	d := host.Sub(c.Now()) // over 0: the guest is behind
	switch {
	case d > -clockTolerance && d < clockTolerance:
		return "", nil
	case d > 0 || d <= -maxSlew:
		return fmt.Sprintf("clock stepped by %v", d), c.Step(host)
	default:
		return fmt.Sprintf("clock slewed by %v", d), c.Slew(d)
	}
}

func (c *ControlServer) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// ServeConn serves one host connection until it ends.
func (c *ControlServer) ServeConn(conn io.ReadWriteCloser) error {
	s := agentproto.NewSession(conn, conn, false)
	defer s.Close()
	frozen := &connFreezes{}
	defer c.thawConn(frozen)
	hello := false
	for b := range s.Control() {
		m, err := agentproto.Decode(b)
		if err != nil {
			return err
		}
		if m.Type != agentproto.TypeHello && !hello {
			return fmt.Errorf("control: %q before a hello", m.Type)
		}
		switch m.Type {
		case agentproto.TypeHello:
			if m.Version != agentproto.Version {
				return fmt.Errorf("control: the host speaks version %d, not %d", m.Version, agentproto.Version)
			}
			hello = true
			if err := s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version}); err != nil {
				return err
			}
		case agentproto.TypeClock:
			if m.Time <= 0 {
				continue
			}
			did, err := adjustClock(c.Clock, time.Unix(0, m.Time))
			if err != nil {
				c.logf("setting the clock: %v", err)
			} else if did != "" {
				c.logf("%s", did)
			}
		case agentproto.TypeBoot:
			if m.Boot == nil {
				return errors.New("control: a boot with no spec")
			}
			spec := *m.Boot
			// In the background: a first boot installs Claude Code, and
			// the clock is still this connection's to set meanwhile.
			go func() {
				ready := agentproto.Message{Type: agentproto.TypeReady}
				if err := c.boot(spec); err != nil {
					ready.Error = err.Error()
				}
				_ = s.Send(ready)
			}()
		case agentproto.TypeVolume:
			if m.VolumeOp == nil {
				return errors.New("control: a volume step with none")
			}
			op := *m.VolumeOp
			// In the background: a filesystem's check and shrink take
			// long, and the connection's other messages keep coming.
			go func() {
				info := c.volumeStep(op)
				if op.Op == agentproto.VolumeFreeze && info.Error == "" && !frozen.add(op) {
					// The connection ended while the freeze ran: no thaw
					// can come on it.
					c.thawOne(op)
					return
				}
				_ = s.Send(agentproto.Message{Type: agentproto.TypeVolume, VolumeInfo: &info})
			}()
		case agentproto.TypeShutdown:
			c.logf("shutting down, as the host asked")
			c.Guest.Shutdown()
			return nil
		}
	}
	// The host hanging up is how a connection ends.
	if err := s.Err(); !errors.Is(err, agentproto.ErrClosed) {
		return err
	}
	return nil
}

// volumeStep does op, when the guest resizes volumes and no other step
// runs.
func (c *ControlServer) volumeStep(op agentproto.VolumeOp) agentproto.VolumeInfo {
	g, ok := c.Guest.(VolumeGuest)
	if !ok {
		return agentproto.VolumeInfo{Op: op.Op, Error: "this guest resizes no volumes"}
	}
	// A checkpoint's freeze and thaw take no turn among a resize's steps:
	// a thaw must never wait, and a freeze of an unmounted volume fails.
	if op.Op != agentproto.VolumeFreeze && op.Op != agentproto.VolumeThaw {
		if !c.volume.TryLock() {
			return agentproto.VolumeInfo{Op: op.Op, Error: "another step of a volume's resize is running in the guest"}
		}
		defer c.volume.Unlock()
	}
	c.logf("volume %s: %s", op.Name, op.Op)
	info := g.Volume(op)
	if info.Error != "" {
		c.logf("volume %s: %s: failed: %s", op.Name, op.Op, info.Error)
	} else {
		c.logf("volume %s: %s: done", op.Name, op.Op)
	}
	return info
}

// boot boots the guest once; a later boot, from this connection or
// another, gets the first one's outcome, after waiting for it.
func (c *ControlServer) boot(spec agentproto.BootSpec) error {
	c.once.Do(func() {
		c.logf("booting %s", spec.Hostname)
		c.bootErr = c.Guest.Boot(spec)
		if c.bootErr != nil {
			c.logf("boot failed: %v", c.bootErr)
		} else {
			c.logf("ready")
		}
	})
	return c.bootErr
}

// connFreezes are the freezes one connection made, which it thaws when it
// ends: a host gone away sends no thaw.
type connFreezes struct {
	mu     sync.Mutex
	ended  bool
	frozen []agentproto.VolumeOp
}

// add records the freeze op, unless the connection has ended.
func (f *connFreezes) add(op agentproto.VolumeOp) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ended {
		return false
	}
	f.frozen = append(f.frozen, op)
	return true
}

// thawConn thaws what the ended connection froze; one already thawed is
// left alone (its token no longer matches).
func (c *ControlServer) thawConn(f *connFreezes) {
	f.mu.Lock()
	f.ended = true
	ops := f.frozen
	f.frozen = nil
	f.mu.Unlock()
	for _, op := range ops {
		c.thawOne(op)
	}
}

// thawOne thaws what the freeze op froze, unless it is thawed already.
func (c *ControlServer) thawOne(op agentproto.VolumeOp) {
	op.Op = agentproto.VolumeThaw
	info := c.volumeStep(op)
	if info.Error == "" && !info.Expired {
		c.logf("volume %s: thawed, as the connection that froze it ended", op.Name)
	}
}
