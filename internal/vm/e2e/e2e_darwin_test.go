package e2e

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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

const ready = "/run/caboose-e2e-ready"

// host is the VMHost the launcher will have, over DIR's files: root.img
// and empty-ext4.img, unless root and empty name others, and the image's
// env, unless env does.
type host struct {
	dir  string
	work string

	root, empty string
	env         []string
}

func (h host) Image(string) (backend.VMImage, error) {
	root, env := h.root, h.env
	if root == "" {
		root = filepath.Join(h.dir, "root.img")
	}
	if env == nil {
		env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
	}
	return backend.VMImage{
		ID:   "e2e",
		Disk: root,
		User: "0:0",
		Env:  env,
		Entrypoint: []string{"/bin/sh", "-c",
			`echo "entrypoint: $(id -u) on $(hostname)"; touch ` + ready + `; exec sleep 2147483647`},
	}, nil
}

func (h host) Machine() (vm.Machine, error) {
	initramfs, err := h.initramfs()
	if err != nil {
		return vm.Machine{}, err
	}
	return vm.Machine{Kernel: filepath.Join(h.dir, "Image"), Initramfs: initramfs, CPUs: 2, MemoryMiB: 1024, Network: "nat"}, nil
}

// initramfs writes the guest's init from DIR's agent, as the launcher will
// from the one it embeds.
func (h host) initramfs() (string, error) {
	agent, err := os.ReadFile(filepath.Join(h.dir, "caboose-agent-linux-arm64"))
	if err != nil {
		return "", err
	}
	initramfs := filepath.Join(h.work, "initramfs.cpio")
	f, err := os.Create(initramfs)
	if err != nil {
		return "", err
	}
	werr := vm.WriteInitramfs(f, agent)
	return initramfs, errors.Join(werr, f.Close())
}

func (h host) NewScratch(path string) error {
	_ = os.Remove(path)
	empty := h.empty
	if empty == "" {
		empty = filepath.Join(h.dir, "empty-ext4.img")
	}
	return vm.CloneFile(empty, path)
}

func (h host) Volume(name string) (string, error) { return "/volumes/" + name + ".img", nil }

// StartVMM is what the launcher will do: caboose-vmm, detached, its output
// to vmm.log.
func (h host) StartVMM(dir vm.Dir) error {
	log, err := os.OpenFile(dir.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(filepath.Join(h.dir, "caboose-vmm"), string(dir))
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func step(t *testing.T, since time.Time, format string, args ...any) {
	t.Helper()
	t.Logf("[%6.2fs] %s", time.Since(since).Seconds(), fmt.Sprintf(format, args...))
}

func TestVM(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" {
		t.Skip("CABOOSE_VM_E2E names no directory of VM files")
	}
	// Short, under /tmp: vm.sock's path must fit a Unix socket's.
	work, err := os.MkdirTemp("/tmp", "cvm")
	if err != nil {
		t.Fatal(err)
	}
	vmdir := vm.Dir(filepath.Join(work, "vm"))
	share := filepath.Join(work, "share")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, "hello.txt"), []byte("from the mac\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := host{dir: dir, work: work}
	v := backend.NewVM("e2e", vmdir, h)
	v.Self, _ = os.Executable()
	v.Ready = ready
	// Keep the logs next to DIR's files, whatever happens.
	keepLogs := func() {
		for _, f := range []string{vmdir.Log(), vmdir.Console()} {
			if b, err := os.ReadFile(f); err == nil {
				_ = os.WriteFile(filepath.Join(dir, filepath.Base(f)), b, 0o644)
			}
		}
	}
	defer func() {
		keepLogs()
		_ = v.Remove()
		os.RemoveAll(work)
	}()

	start := time.Now()
	spec := backend.Spec{
		Image: "e2e", Hostname: "caboose-e2e",
		Env:    []string{"IS_SANDBOX=1", "TZ=UTC"},
		User:   "0:0",
		Mounts: []backend.Mount{{Source: share, Target: "/work"}},
		Labels: []string{"caboose.isolation=vm"},
	}
	if err := v.Create(spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	step(t, start, "created; state %s", v.State())
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
	step(t, start, "ready")

	out := func(argv ...string) string {
		t.Helper()
		s, err := backend.Output(v, argv...)
		if err != nil {
			t.Errorf("%q: %v", argv, err)
		}
		return s
	}
	step(t, start, "uname: %s", out("uname", "-a"))
	if got := out("hostname"); got != "caboose-e2e" {
		t.Errorf("hostname %q", got)
	}
	if got := out("sh", "-c", "echo $IS_SANDBOX $TZ"); got != "1 UTC" {
		t.Errorf("env %q", got)
	}
	if got := out("cat", "/work/hello.txt"); got != "from the mac" {
		t.Errorf("the share: %q", got)
	}
	out("sh", "-c", "echo from the guest > /work/back.txt")
	if b, err := os.ReadFile(filepath.Join(share, "back.txt")); err != nil || string(b) != "from the guest\n" {
		t.Errorf("a guest write on the Mac: %q, %v", b, err)
	}
	cmd := v.Command(backend.ExecSpec{Argv: []string{"sh", "-c", "exit 7"}})
	_ = cmd.Run()
	if c := cmd.ProcessState.ExitCode(); c != 7 {
		t.Errorf("status %d, want 7", c)
	}
	if got, err := backend.Capture(v, backend.ExecSpec{Argv: []string{"tr", "a-z", "A-Z"}, Stdin: true}, strings.NewReader("stdin")); err != nil || got != "STDIN" {
		t.Errorf("stdin: %q, %v", got, err)
	}
	if got, _ := backend.Capture(v, backend.ExecSpec{Argv: []string{"tty"}, Stdin: true, TTY: true}, strings.NewReader("")); !strings.Contains(got, "/dev/pts/") {
		t.Errorf("a terminal: %q", got)
	}
	if guest, err := strconv.ParseInt(out("date", "+%s"), 10, 64); err != nil || abs(guest-time.Now().Unix()) > 2 {
		t.Errorf("the guest's clock: %d, the Mac's %d (%v)", guest, time.Now().Unix(), err)
	}
	step(t, start, "network: %s", out("sh", "-c", "ip -4 -o addr show eth0 2>&1 | head -1; cat /etc/resolv.conf | tr '\\n' ' '"))
	step(t, start, "dns: %s", out("sh", "-c", "nslookup example.com 2>&1 | tail -2 | tr '\\n' ' ' || true"))

	// The link port answers with the agent's hello.
	c, err := v.DialLink()
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	s := agentproto.NewSession(c, c, true)
	_ = s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version})
	select {
	case b := <-s.Control():
		m, err := agentproto.Decode(b)
		if err != nil || m.Type != agentproto.TypeHello || m.Version != agentproto.Version {
			t.Errorf("link hello: %+v, %v", m, err)
		}
	case <-time.After(10 * time.Second):
		t.Error("no hello on the link")
	}
	s.Close()
	step(t, start, "commands, share, clock and link checked")

	out("touch", "/tmp/marker")
	t0 := time.Now()
	if err := v.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	step(t, start, "stopped in %.2fs; state %s", time.Since(t0).Seconds(), v.State())
	if b, _ := os.ReadFile(vmdir.Log()); !strings.Contains(string(b), "powered off") {
		t.Errorf("vmm.log says no power-off:\n%s", b)
	}

	t0 = time.Now()
	if err := v.Start(); err != nil {
		t.Fatalf("start again: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready again: %v", err)
	}
	step(t, start, "started again, ready in %.2fs", time.Since(t0).Seconds())
	if got := out("cat", "/work/back.txt"); got != "from the guest" {
		t.Errorf("after a restart: %q", got)
	}
	if got := out("sh", "-c", "ls /tmp/marker 2>&1 || true"); !strings.Contains(got, "No such file") {
		t.Errorf("the scratch disk survived a restart: %q", got)
	}
	keepLogs()
	if err := v.Remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	step(t, start, "removed; state %s", v.State())
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
