// Package vmm is caboose-vmm, whatever runs the VM: the detached process
// that owns one VM for as long as it runs. It boots the VM of the
// directory's machine.json on a Runner (Virtualization.framework on a Mac,
// internal/vm/vz), serves vm.sock, keeps the guest's clock on the host's,
// and on SIGTERM asks the guest to shut down. It holds no policy: what the
// guest boots into is the launcher's, sent over the control port through
// vm.sock, and vmm never reads it.
package vmm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/hvsock"
	"github.com/bfreis/caboose/internal/linkdebug"
	"github.com/bfreis/caboose/internal/vm"
)

// Runner is one VM on some hypervisor.
type Runner interface {
	// Start boots m and returns once the VM runs.
	Start(m vm.Machine) error
	// Connect opens a connection to a vsock port in the guest; an error
	// is typically nothing listening there yet.
	Connect(port uint32) (io.ReadWriteCloser, error)
	// Stop stops the VM at once, as pulling its plug does.
	Stop() error
	// Done is closed once the VM has stopped, and Err is then why: nil
	// when the guest powered off.
	Done() <-chan struct{}
	Err() error
}

// How long the guest has to power off once asked (docker stop gives a
// container ten seconds, and the agent gives the entrypoint as much), and
// how long a hard stop may take.
var (
	shutdownGrace = 15 * time.Second
	stopTimeout   = 10 * time.Second
)

// Run boots dir's VM on r and serves it until the VM stops, or until ctx
// ends, which shuts the guest down first.
func Run(ctx context.Context, dir vm.Dir, r Runner, logf func(format string, args ...any)) error {
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	var m vm.Machine
	b, err := os.ReadFile(dir.Machine())
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", dir.Machine(), err)
	}
	if err := os.WriteFile(dir.PID(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return err
	}
	defer os.Remove(dir.PID())

	mac := m.MAC
	if mac == "" {
		mac = "random"
	}
	if k := linkdebug.Get(); k.Raw != "" {
		logf("%s=%q: %+v", linkdebug.Var, k.Raw, k)
	}
	logf("booting: %d cpus, %d MiB, %d disks, %d shares, network %s, MAC %s", m.CPUs, m.MemoryMiB, len(m.Disks), len(m.Shares), m.Network, mac)
	if err := r.Start(m); err != nil {
		return err
	}
	logf("the VM runs")

	_ = os.Remove(dir.Socket())
	ln, err := net.Listen("unix", dir.Socket())
	if err != nil {
		_ = r.Stop()
		return err
	}
	defer os.Remove(dir.Socket())
	defer ln.Close()
	if err := os.Chmod(dir.Socket(), 0o600); err != nil {
		_ = r.Stop()
		return err
	}
	go serve(ln, r)

	kctx, stopKeeper := context.WithCancel(context.Background())
	defer stopKeeper()
	keeper := &vm.ClockKeeper{
		Dial: func() (*vm.Control, error) { return controlWhenUp(r) },
		Logf: logf,
	}
	go keeper.Run(kctx)

	select {
	case <-r.Done():
	case <-ctx.Done():
		logf("asked to stop: shutting the guest down")
		shutdown(r, logf)
	}
	if err := r.Err(); err != nil {
		logf("the VM stopped: %v", err)
		return err
	}
	logf("the VM powered off")
	return nil
}

// lock makes this the one vmm of dir.
func lock(dir vm.Dir) (func(), error) {
	f, err := os.OpenFile(dir.Lock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another caboose-vmm already runs the VM in %s", string(dir))
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

// control connects to the guest's control port directly, not through
// vm.sock.
func control(r Runner) (*vm.Control, error) {
	c, err := r.Connect(agentproto.PortControl)
	if err != nil {
		return nil, err
	}
	return vm.NewControl(c, 5*time.Second)
}

// bootWait is how long the guest's agent has to listen once the VM runs:
// the kernel boots first, which takes well under a second (spike 1).
var bootWait = 30 * time.Second

// controlWhenUp is control, retried while the guest is still booting.
func controlWhenUp(r Runner) (*vm.Control, error) {
	deadline := time.Now().Add(bootWait)
	for {
		c, err := control(r)
		if err == nil || time.Now().After(deadline) {
			return c, err
		}
		select {
		case <-r.Done():
			return nil, err
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// shutdown asks the agent to stop the sandbox and power off, and pulls the
// plug when that does not happen in time.
func shutdown(r Runner, logf func(format string, args ...any)) {
	if c, err := control(r); err != nil {
		logf("cannot reach the agent to shut down (%v): stopping the VM", err)
	} else {
		_ = c.Shutdown()
		c.Close()
		select {
		case <-r.Done():
			return
		case <-time.After(shutdownGrace):
			logf("the guest did not power off in %v: stopping the VM", shutdownGrace)
		}
	}
	if err := r.Stop(); err != nil {
		logf("stopping the VM: %v", err)
	}
	select {
	case <-r.Done():
	case <-time.After(stopTimeout):
		logf("the VM did not stop")
	}
}

// serve answers each connection to vm.sock with the guest port it asks
// for, as hybrid vsock does: nothing listening there closes it.
func serve(ln net.Listener, r Runner) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			port, err := hvsock.Accept(c, 5*time.Second)
			if err != nil {
				c.Close()
				return
			}
			g, err := r.Connect(port)
			if err != nil {
				c.Close()
				return
			}
			if err := hvsock.Ready(c, port); err != nil {
				c.Close()
				g.Close()
				return
			}
			sockBuffers(c)
			sockBuffers(g)
			splice(c, g)
		}()
	}
}

// relayBuffer is what one copy of splice moves at a time, and sockBuffer
// the socket buffers it asks for on both sides: a Mac's Unix sockets take
// 8 KiB unless asked, and io.Copy 32 KiB.
const (
	relayBuffer = 256 << 10
	sockBuffer  = 1 << 20
)

// sockBuffers asks for sockBuffer each way on c, when it is a socket; the
// system may give less.
func sockBuffers(c any) {
	sc, ok := c.(syscall.Conn)
	if !ok || linkdebug.Get().NoSockBuf {
		return
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, sockBuffer)
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, sockBuffer)
	})
}

// splice copies both ways until both are done, passing each side's end of
// input on as a half-close, and then closes both. Each way copies through
// a relayBuffer of its own, never io.Copy's fallback of 32 KiB, which an
// *os.File's ReadFrom and WriteTo take on a Mac.
func splice(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	one := func(dst, src io.ReadWriteCloser) {
		defer wg.Done()
		if linkdebug.Get().NoRelayBuf {
			_, _ = io.Copy(dst, src)
		} else {
			_, _ = io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, make([]byte, relayBuffer))
		}
		closeWrite(dst)
	}
	go one(a, b)
	go one(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

// closeWrite ends c's writing side, or all of c when it has none of its
// own: a socket from a hypervisor is an *os.File.
func closeWrite(c io.ReadWriteCloser) {
	switch x := c.(type) {
	case interface{ CloseWrite() error }:
		_ = x.CloseWrite()
	case syscall.Conn:
		rc, err := x.SyscallConn()
		if err != nil {
			c.Close()
			return
		}
		_ = rc.Control(func(fd uintptr) { _ = syscall.Shutdown(int(fd), syscall.SHUT_WR) })
	default:
		c.Close()
	}
}
