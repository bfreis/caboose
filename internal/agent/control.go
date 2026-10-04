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
}

// The clock is left alone within clockTolerance of the host's. Behind by
// more, it steps forward: a sleep of the host's froze it, and a file's
// time never goes back that way. Ahead, it slews back up to maxSlew, so it
// never runs backwards under a build; ahead by more is a clock gone wrong,
// and steps. (Spike 5: asleep, a guest loses exactly the time slept, and
// awake it drifts about 1.5 ppm.)
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
