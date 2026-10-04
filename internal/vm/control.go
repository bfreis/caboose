package vm

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/hvsock"
)

// Control is the host's end of a connection to the guest's control port
// (agentproto's control.go).
type Control struct {
	s *agentproto.Session
}

// DialControl connects to the control port behind socket and exchanges
// hellos, all within timeout.
func DialControl(socket string, timeout time.Duration) (*Control, error) {
	c, err := hvsock.Dial(socket, agentproto.PortControl, timeout)
	if err != nil {
		return nil, err
	}
	return NewControl(c, timeout)
}

// NewControl says hello on conn, a connection to the control port, and
// waits up to timeout for the agent's.
func NewControl(conn io.ReadWriteCloser, timeout time.Duration) (*Control, error) {
	c := &Control{s: agentproto.NewSession(conn, conn, true)}
	if err := c.s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version}); err != nil {
		c.Close()
		return nil, err
	}
	m, err := c.next(timeout)
	if err != nil {
		c.Close()
		return nil, err
	}
	if m.Type != agentproto.TypeHello {
		c.Close()
		return nil, fmt.Errorf("control: the agent sent %q before its hello", m.Type)
	}
	if m.Version != agentproto.Version {
		c.Close()
		return nil, fmt.Errorf("control: the agent speaks version %d, not %d (the VM's image is from another caboose)", m.Version, agentproto.Version)
	}
	return c, nil
}

// next is the agent's next message, within timeout (0: no limit).
func (c *Control) next(timeout time.Duration) (agentproto.Message, error) {
	var expired <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		expired = t.C
	}
	select {
	case b, ok := <-c.s.Control():
		if !ok {
			err := c.s.Err()
			if err == nil || errors.Is(err, agentproto.ErrClosed) {
				err = errors.New("the agent closed the connection")
			}
			return agentproto.Message{}, fmt.Errorf("control: %w", err)
		}
		return agentproto.Decode(b)
	case <-expired:
		return agentproto.Message{}, errors.New("control: the agent did not answer")
	}
}

// Clock sets the guest's clock to t.
func (c *Control) Clock(t time.Time) error {
	return c.s.Send(agentproto.Message{Type: agentproto.TypeClock, Time: t.UnixNano()})
}

// Boot asks the agent to boot the sandbox; Ready says when it has.
func (c *Control) Boot(spec agentproto.BootSpec) error {
	s := spec
	return c.s.Send(agentproto.Message{Type: agentproto.TypeBoot, Boot: &s})
}

// Ready waits for the answer to Boot, up to timeout (0: no limit): nil
// once the sandbox is ready, else why it could not be made so.
func (c *Control) Ready(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		if timeout == 0 {
			left = 0
		} else if left <= 0 {
			return errors.New("control: not ready in time")
		}
		m, err := c.next(left)
		if err != nil {
			return err
		}
		if m.Type != agentproto.TypeReady {
			continue
		}
		if m.Error != "" {
			return fmt.Errorf("the guest could not boot: %.500s", m.Error)
		}
		return nil
	}
}

// Shutdown asks the agent to stop the sandbox and power the VM off.
func (c *Control) Shutdown() error {
	return c.s.Send(agentproto.Message{Type: agentproto.TypeShutdown})
}

// Done is closed when the connection ends.
func (c *Control) Done() <-chan struct{} { return c.s.Done() }

// Close ends the connection. What was sent before it still reaches the
// agent, which reads its messages to the end.
func (c *Control) Close() error { return c.s.Close() }
