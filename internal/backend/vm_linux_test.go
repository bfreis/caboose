package backend

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agent"
	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/hvsock"
	"github.com/bfreis/caboose/internal/vm"
)

// The test binary is also the exec helper, as the launcher is.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == vm.ExecHelper {
		os.Exit(vm.ExecMain(os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// fakeVMM is a VMHost whose vmm is a goroutine: it serves vm.sock with the
// real agent's control and exec servers, over a guest that boots at once
// and runs commands as the test's user. Its link port echoes.
type fakeVMM struct {
	t   *testing.T
	img VMImage

	mu       sync.Mutex
	scratch  int
	machines []vm.Machine
	boots    []agentproto.BootSpec
	clocks   int
	volumes  int
	ln       net.Listener
}

func (f *fakeVMM) Image(ref string) (VMImage, error) { return f.img, nil }

func (f *fakeVMM) Machine() (vm.Machine, error) {
	return vm.Machine{Kernel: "/k", Initramfs: "/i", CPUs: 2, MemoryMiB: 1024}, nil
}

func (f *fakeVMM) NewScratch(path string) error {
	f.mu.Lock()
	f.scratch++
	f.mu.Unlock()
	return os.WriteFile(path, nil, 0o600)
}

func (f *fakeVMM) Volume(name string) (string, error) {
	f.mu.Lock()
	f.volumes++
	f.mu.Unlock()
	return "/volumes/" + name + ".img", nil
}

func (f *fakeVMM) StartVMM(dir vm.Dir) error {
	b, err := os.ReadFile(dir.Machine())
	if err != nil {
		return err
	}
	var m vm.Machine
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	ln, err := net.Listen("unix", dir.Socket())
	if err != nil {
		return err
	}
	if err := os.WriteFile(dir.Console(), []byte("kernel\ninit\nready\n"), 0o600); err != nil {
		return err
	}
	g := &fakeGuest{f: f}
	f.mu.Lock()
	f.machines = append(f.machines, m)
	f.ln = ln
	f.mu.Unlock()
	ctl := &agent.ControlServer{Guest: g, Clock: fakeClock{f}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c, g, ctl)
		}
	}()
	return nil
}

func (f *fakeVMM) serve(c net.Conn, g *fakeGuest, ctl *agent.ControlServer) {
	defer c.Close()
	port, err := hvsock.Accept(c, time.Second)
	if err != nil || hvsock.Ready(c, 1) != nil {
		return
	}
	switch port {
	case agentproto.PortControl:
		_ = ctl.ServeConn(c)
	case agentproto.PortExec:
		srv := &agent.ExecServer{Refuse: "the sandbox has not booted yet"}
		if spec := g.booted(); spec != nil {
			srv = &agent.ExecServer{Env: spec.Env, Dir: "/"}
		}
		_ = srv.ServeConn(c)
	case agentproto.PortLink:
		_, _ = io.Copy(c, c)
	}
}

// stop is the VM powering off: vmm ends, and its socket goes.
func (f *fakeVMM) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln != nil {
		f.ln.Close()
		f.ln = nil
	}
}

type fakeGuest struct {
	f    *fakeVMM
	mu   sync.Mutex
	spec *agentproto.BootSpec
}

func (g *fakeGuest) Boot(s agentproto.BootSpec) error {
	g.f.mu.Lock()
	g.f.boots = append(g.f.boots, s)
	g.f.mu.Unlock()
	g.mu.Lock()
	g.spec = &s
	g.mu.Unlock()
	return nil
}

func (g *fakeGuest) booted() *agentproto.BootSpec {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.spec
}

func (g *fakeGuest) Shutdown() { g.f.stop() }

// fakeClock counts the host's clock messages; the guest is always behind.
type fakeClock struct{ f *fakeVMM }

func (c fakeClock) Now() time.Time { return time.Unix(0, 0) }
func (c fakeClock) Step(time.Time) error {
	c.f.mu.Lock()
	c.f.clocks++
	c.f.mu.Unlock()
	return nil
}
func (c fakeClock) Slew(time.Duration) error { return nil }

func newTestVM(t *testing.T) (*VM, *fakeVMM) {
	t.Helper()
	// Short: a Unix socket's path has a limit.
	dir, err := os.MkdirTemp("", "vm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeVMM{t: t, img: VMImage{
		ID: "sha256:img", Disk: "/images/root-abc.img", User: "1000:1000",
		Labels:     map[string]string{"caboose.compat": "3", "caboose.isolation": "image's"},
		Env:        []string{"PATH=" + os.Getenv("PATH"), "HOME=/home/agent"},
		Entrypoint: []string{"/usr/local/bin/entrypoint.sh"},
	}}
	v := NewVM("box", vm.Dir(filepath.Join(dir, "box")), f)
	v.Self, err = os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	v.Ready = "/run/ready"
	v.Stderr = io.Discard
	t.Cleanup(f.stop)
	return v, f
}

// Create, a command, the link, stop, start again and remove: the whole
// life of a VM, against the agent's own servers.
func TestVMLifecycle(t *testing.T) {
	v, f := newTestVM(t)
	if s := v.State(); s != "absent" {
		t.Fatalf("state %s before create", s)
	}
	repo := t.TempDir()
	spec := Spec{
		Image: "caboose", Hostname: "caboose",
		Env:     []string{"HOME=/root", "IS_SANDBOX=1"},
		User:    "0:0",
		Mounts:  []Mount{{Source: repo, Target: "/work"}},
		Volumes: []Volume{{Name: "docker", Target: "/var/lib/docker"}},
		Labels:  []string{"caboose.isolation=vm", "caboose.hash=abc=def"},
		Egress:  agentproto.EgressListen,
	}
	if err := v.Create(spec); err != nil {
		t.Fatal(err)
	}
	if s := v.State(); s != "running" {
		t.Fatalf("state %s after create", s)
	}
	if err := v.WaitReady(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	boot, m, clocks := f.boots[0], f.machines[0], f.clocks
	f.mu.Unlock()
	want := agentproto.BootSpec{
		Hostname: "caboose",
		Env:      []string{"PATH=" + os.Getenv("PATH"), "HOME=/root", "IS_SANDBOX=1"},
		User:     "0:0",
		Mounts:   []agentproto.GuestMount{{Tag: vm.ShareTag, Path: "m0", Target: "/work"}},
		Disks:    []agentproto.GuestDisk{{Device: "/dev/vdc", Target: "/var/lib/docker"}},
		Cmd:      []string{"/usr/local/bin/entrypoint.sh"},
		Ready:    "/run/ready",
		Egress:   agentproto.EgressListen,
	}
	if !reflect.DeepEqual(boot, want) {
		t.Errorf("boot spec\n got %+v\nwant %+v", boot, want)
	}
	if clocks == 0 {
		t.Error("no clock before the boot")
	}
	wantDisks := []vm.Disk{{Path: "/images/root-abc.img", ReadOnly: true}, {Path: v.Dir.Scratch()}, {Path: "/volumes/docker.img"}}
	if !reflect.DeepEqual(m.Disks, wantDisks) || m.Network != "nat" || m.Console != v.Dir.Console() ||
		!reflect.DeepEqual(m.Shares, []vm.Share{{Name: "m0", Path: repo}}) || m.CPUs != 2 || m.MAC != v.Dir.MAC() {
		t.Errorf("machine %+v", m)
	}

	labels, err := v.Labels()
	if err != nil || labels["caboose.isolation"] != "vm" || labels["caboose.hash"] != "abc=def" || labels["caboose.compat"] != "3" {
		t.Errorf("labels %v, %v", labels, err)
	}
	if v.Image() != "sha256:img" {
		t.Errorf("image %q", v.Image())
	}
	if ms, err := v.Mounts(); err != nil || !reflect.DeepEqual(ms, spec.Mounts) {
		t.Errorf("mounts %v, %v", ms, err)
	}
	var logs bytes.Buffer
	if err := v.Logs(&logs, nil, 2); err != nil || logs.String() != "init\nready\n" {
		t.Errorf("logs %q, %v", logs.String(), err)
	}

	// A command: its output, its errors, its status, and its stdin.
	out, err := Output(v, "sh", "-c", "echo out; echo $IS_SANDBOX")
	if err != nil || out != "out\n1" {
		t.Errorf("output %q, %v", out, err)
	}
	err = Quiet(v, "sh", "-c", "echo no >&2; exit 3")
	if err == nil {
		t.Error("exit 3 was a success")
	}
	_, err = Output(v, "sh", "-c", "echo no >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "no") {
		t.Errorf("stderr not in the error: %v", err)
	}
	cmd := v.Command(ExecSpec{Argv: []string{"sh", "-c", "exit 3"}})
	if err := cmd.Run(); cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 3 {
		t.Errorf("status: %v", err)
	}
	got, err := Capture(v, ExecSpec{Argv: []string{"tr", "a-z", "A-Z"}, Stdin: true}, strings.NewReader("hello"))
	if err != nil || got != "HELLO" {
		t.Errorf("stdin: %q, %v", got, err)
	}
	if err := Quiet(v, "no-such-command-anywhere"); err == nil {
		t.Error("a missing command ran")
	}

	// The link port.
	l, ok := any(v).(Linker)
	if !ok {
		t.Fatal("a VM is no Linker")
	}
	c, err := l.DialLink()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("link"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "link" {
		t.Errorf("link echoed %q, %v", buf, err)
	}
	c.Close()

	if err := v.Stop(); err != nil {
		t.Fatal(err)
	}
	if s := v.State(); s != "exited" {
		t.Fatalf("state %s after stop", s)
	}
	if err := v.Stop(); err != nil {
		t.Fatalf("stopping a stopped VM: %v", err)
	}
	if err := v.Create(spec); err == nil {
		t.Fatal("created over an existing VM")
	}

	if err := v.Start(); err != nil {
		t.Fatal(err)
	}
	if s := v.State(); s != "running" {
		t.Fatalf("state %s after start", s)
	}
	if err := v.WaitReady(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	scratch, boots, volumes := f.scratch, len(f.boots), f.volumes
	f.mu.Unlock()
	if scratch != 2 {
		t.Errorf("%d scratch disks for two starts", scratch)
	}
	// Asked for at create and again at each start, which makes a deleted
	// one afresh.
	if volumes != 3 {
		t.Errorf("the volume asked for %d times in a create and two starts", volumes)
	}
	if boots != 2 {
		t.Errorf("%d boots for two starts", boots)
	}

	if err := v.Remove(); err != nil {
		t.Fatal(err)
	}
	if s := v.State(); s != "absent" {
		t.Fatalf("state %s after remove", s)
	}
	if _, err := os.Stat(string(v.Dir)); !os.IsNotExist(err) {
		t.Errorf("the VM's dir is still there: %v", err)
	}
}

// Before the boot is done, a command is refused, and says why.
func TestVMExecBeforeBoot(t *testing.T) {
	v, f := newTestVM(t)
	if err := v.Dir.WriteState(vm.State{Image: "i"}); err != nil {
		t.Fatal(err)
	}
	if err := v.Dir.WriteMachine(vm.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := f.StartVMM(v.Dir); err != nil {
		t.Fatal(err)
	}
	_, err := Output(v, "true")
	if err == nil || !strings.Contains(err.Error(), "not booted") {
		t.Fatalf("err = %v", err)
	}
}

// With no vmm serving, a command says it cannot reach the VM, as docker's
// own failures do, with status 125.
func TestVMExecNoVMM(t *testing.T) {
	v, _ := newTestVM(t)
	cmd := v.Command(ExecSpec{Argv: []string{"true"}})
	var errb bytes.Buffer
	cmd.Stderr = &errb
	_ = cmd.Run()
	if cmd.ProcessState.ExitCode() != vm.ExecFailed || !strings.Contains(errb.String(), "cannot reach the sandbox's VM") {
		t.Fatalf("status %d, said %q", cmd.ProcessState.ExitCode(), errb.String())
	}
}
