package vm

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/hvsock"
)

// How long a VM has to come up: vmm to serve its socket, the agent to
// listen once the kernel has booted, and the VM to go once asked to.
var (
	vmmTimeout   = 30 * time.Second
	agentTimeout = 60 * time.Second
	stopTimeout  = 20 * time.Second
	killTimeout  = 15 * time.Second
	pollEvery    = 100 * time.Millisecond
	// dialEvery is how soon the control port is tried again: the boot
	// waits on it, and a refused dial costs vmm and the guest next to
	// nothing, where pollEvery's 100 ms held a boot up by up to as much
	// once the agent listened.
	dialEvery = 10 * time.Millisecond
)

// Serving reports whether a vmm answers on d's vm.sock.
func (d Dir) Serving() bool {
	c, err := net.DialTimeout("unix", d.Socket(), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Boot starts d's VM: m written for vmm, with d's own MAC unless m has
// one, vmm started by start, and once the agent listens, the host's clock
// and spec. It returns once the agent has the spec, not once the guest is
// ready (WaitReady).
func (d Dir) Boot(m Machine, spec agentproto.BootSpec, start func(Dir) error) error {
	_ = os.Remove(d.Socket())
	if m.MAC == "" {
		m.MAC = d.MAC()
	}
	if err := d.WriteMachine(m); err != nil {
		return err
	}
	if err := start(d); err != nil {
		return fmt.Errorf("cannot start the VM: %w", err)
	}
	// vmm writes its pid first and removes it as it ends: one seen and
	// gone again is a vmm that failed, with no reason to wait on.
	sawPID, ended := false, false
	up := waitFor(vmmTimeout, func() bool {
		if d.Serving() {
			return true
		}
		_, err := os.Stat(d.PID())
		sawPID = sawPID || err == nil
		ended = sawPID && err != nil
		return ended
	})
	if !up || ended {
		return fmt.Errorf("the VM did not start (what vmm said: %s; the guest's console: %s)", d.Log(), d.Console())
	}
	c, err := d.Control(agentTimeout)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Clock(time.Now()); err != nil {
		return err
	}
	return c.Boot(spec)
}

// Control connects to the agent's control port, waiting up to timeout for
// it to listen: the kernel boots first.
func (d Dir) Control(timeout time.Duration) (*Control, error) {
	deadline := time.Now().Add(timeout)
	for {
		c, err := DialControl(d.Socket(), 5*time.Second)
		if err == nil {
			return c, nil
		}
		if !errors.Is(err, hvsock.ErrRefused) || time.Now().After(deadline) {
			return nil, fmt.Errorf("cannot reach the agent in the VM: %w (its console: %s)", err, d.Console())
		}
		time.Sleep(dialEvery)
	}
}

// WaitReady waits up to timeout for the guest booted with spec to be
// ready, and says why when it cannot be: the boot's own error.
func (d Dir) WaitReady(spec agentproto.BootSpec, timeout time.Duration) error {
	c, err := d.Control(timeout)
	if err != nil {
		return err
	}
	defer c.Close()
	// A boot of a guest already booting is answered with the first's
	// outcome.
	if err := c.Boot(spec); err != nil {
		return err
	}
	return c.Ready(timeout)
}

// Stop asks the agent to shut the guest down, which ends vmm, and signals
// vmm when it does not end in time.
func (d Dir) Stop() error {
	if !d.Serving() {
		return nil
	}
	if c, err := DialControl(d.Socket(), 5*time.Second); err == nil {
		_ = c.Shutdown()
		c.Close()
		if waitFor(stopTimeout, func() bool { return !d.Serving() }) {
			return nil
		}
	}
	pid, err := d.vmmPID()
	if err != nil {
		return fmt.Errorf("the VM did not stop, and vmm cannot be told to: %v", err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("stopping vmm (pid %d): %v", pid, err)
		}
		if waitFor(killTimeout, func() bool { return !d.Serving() }) {
			return nil
		}
	}
	return fmt.Errorf("vmm (pid %d) did not stop", pid)
}

func (d Dir) vmmPID() (int, error) {
	b, err := os.ReadFile(d.PID())
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("%s holds no pid", d.PID())
	}
	return pid, nil
}

// waitFor polls ok until it holds or timeout passes.
func waitFor(timeout time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if ok() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollEvery)
	}
}
