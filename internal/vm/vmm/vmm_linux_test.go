package vmm

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agent"
	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/vm"
)

// The test binary is also the exec helper, as the launcher is.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == vm.ExecHelper {
		os.Exit(vm.ExecMain(os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// fakeRunner is a VM whose guest is the agent's control and exec servers,
// in this process, reached over socketpairs. Its guest powers off when the
// agent is told to shut down.
type fakeRunner struct {
	mu       sync.Mutex
	started  *vm.Machine
	spec     *agentproto.BootSpec
	ctl      *agent.ControlServer
	done     chan struct{}
	stopped  bool
	err      error
	shutdown bool // the guest honours a shutdown
}

func newRunner() *fakeRunner {
	r := &fakeRunner{done: make(chan struct{}), shutdown: true}
	r.ctl = &agent.ControlServer{Guest: (*fakeGuest)(r), Clock: stuckClock{}}
	return r
}

type fakeGuest fakeRunner

func (g *fakeGuest) Boot(s agentproto.BootSpec) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.spec = &s
	return nil
}

func (g *fakeGuest) Shutdown() {
	r := (*fakeRunner)(g)
	if r.shutdown {
		r.end(nil)
	}
}

type stuckClock struct{}

func (stuckClock) Now() time.Time           { return time.Unix(0, 0) }
func (stuckClock) Step(time.Time) error     { return nil }
func (stuckClock) Slew(time.Duration) error { return nil }

func (r *fakeRunner) end(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.stopped {
		r.stopped, r.err = true, err
		close(r.done)
	}
}

func (r *fakeRunner) Start(m vm.Machine) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = &m
	return nil
}

func (r *fakeRunner) Connect(port uint32) (io.ReadWriteCloser, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	conn := func(fd int) net.Conn {
		f := os.NewFile(uintptr(fd), "pair")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			panic(err)
		}
		return c
	}
	host, guest := conn(fds[0]), conn(fds[1])
	switch port {
	case agentproto.PortControl:
		go func() { _ = r.ctl.ServeConn(guest) }()
	case agentproto.PortExec:
		r.mu.Lock()
		spec := r.spec
		r.mu.Unlock()
		srv := &agent.ExecServer{Refuse: "the sandbox has not booted yet"}
		if spec != nil {
			srv = &agent.ExecServer{Env: spec.Env, Dir: "/"}
		}
		go func() { _ = srv.ServeConn(guest) }()
	default:
		host.Close()
		guest.Close()
		return nil, errors.New("connection reset by peer")
	}
	return host, nil
}

func (r *fakeRunner) Stop() error { r.end(errors.New("stopped by the host")); return nil }

func (r *fakeRunner) Done() <-chan struct{} { return r.done }

func (r *fakeRunner) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// host is a VMHost whose vmm is Run in this process, on a fakeRunner.
type host struct {
	t      *testing.T
	runner *fakeRunner
	cancel context.CancelFunc
	ran    chan error
}

func (h *host) Image(string) (backend.VMImage, error) {
	return backend.VMImage{ID: "img", Disk: "/root.img", User: "0:0",
		Env: []string{"PATH=" + os.Getenv("PATH")}, Entrypoint: []string{"/entrypoint"}}, nil
}

func (h *host) Machine() (vm.Machine, error) { return vm.Machine{CPUs: 1, MemoryMiB: 512}, nil }

func (h *host) NewScratch(path string) error { return os.WriteFile(path, nil, 0o600) }

func (h *host) StartVMM(dir vm.Dir) error {
	h.runner = newRunner()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.ran = make(chan error, 1)
	r := h.runner
	go func() { h.ran <- Run(ctx, dir, r, h.t.Logf) }()
	return nil
}

func (h *host) Volume(name string) (string, error) { return "/volumes/" + name + ".img", nil }

func newVM(t *testing.T) (*backend.VM, *host) {
	t.Helper()
	d, err := os.MkdirTemp("", "vmm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	h := &host{t: t}
	v := backend.NewVM("box", vm.Dir(filepath.Join(d, "box")), h)
	v.Self, _ = os.Executable()
	v.Ready = "/run/ready"
	v.Stderr = io.Discard
	t.Cleanup(func() {
		if h.cancel != nil {
			h.cancel()
		}
	})
	return v, h
}

// Through vmm: the backend boots the guest, runs a command in it, and
// stops it; vmm leaves no socket or pid behind.
func TestRunThroughTheBackend(t *testing.T) {
	v, h := newVM(t)
	if err := v.Create(backend.Spec{Image: "i", Hostname: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := v.WaitReady(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(v.Dir.PID()); err != nil {
		t.Errorf("no pid while running: %v", err)
	}
	out, err := backend.Output(v, "sh", "-c", "echo hi; cat")
	if err != nil || out != "hi" {
		t.Fatalf("output %q, %v", out, err)
	}
	if err := v.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := <-h.ran; err != nil {
		t.Fatalf("vmm: %v", err)
	}
	for _, p := range []string{v.Dir.Socket(), v.Dir.PID()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind", p)
		}
	}
	if v.State() != "exited" {
		t.Errorf("state %s", v.State())
	}
}

// SIGTERM (ctx) asks the guest to shut down; one that does not is
// stopped.
func TestRunShutdown(t *testing.T) {
	for _, honours := range []bool{true, false} {
		v, h := newVM(t)
		if err := v.Create(backend.Spec{Image: "i"}); err != nil {
			t.Fatal(err)
		}
		h.runner.shutdown = honours
		old := shutdownGrace
		shutdownGrace = 100 * time.Millisecond
		h.cancel()
		err := <-h.ran
		shutdownGrace = old
		if honours && err != nil {
			t.Errorf("a guest that powered off: %v", err)
		}
		if !honours && (err == nil || !strings.Contains(err.Error(), "stopped by the host")) {
			t.Errorf("a guest that did not: %v", err)
		}
	}
}

// One vmm per VM: a second finds the lock held.
func TestRunLock(t *testing.T) {
	v, h := newVM(t)
	if err := v.Create(backend.Spec{Image: "i"}); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), v.Dir, newRunner(), t.Logf)
	if err == nil || !strings.Contains(err.Error(), "already runs") {
		t.Fatalf("second vmm: %v", err)
	}
	h.cancel()
	<-h.ran
}

// A vmm that cannot start the VM ends at once, and the backend says so
// without waiting out its timeout.
func TestStartFailsFast(t *testing.T) {
	d, err := os.MkdirTemp("", "vmm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(d)
	v := backend.NewVM("box", vm.Dir(filepath.Join(d, "box")), failingHost{&host{t: t}})
	v.Stderr = io.Discard
	start := time.Now()
	err = v.Create(backend.Spec{Image: "i"})
	if err == nil || !strings.Contains(err.Error(), "vmm.log") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

type failingHost struct{ *host }

func (failingHost) StartVMM(dir vm.Dir) error {
	go func() { _ = Run(context.Background(), dir, failingRunner{newRunner()}, func(string, ...any) {}) }()
	return nil
}

type failingRunner struct{ *fakeRunner }

func (failingRunner) Start(vm.Machine) error {
	time.Sleep(300 * time.Millisecond) // long enough to see the pid
	return errors.New("no entitlement")
}
