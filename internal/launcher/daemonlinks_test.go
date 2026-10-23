package launcher

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/docker"
)

// agentBox is an in-memory sandbox whose link is dialled, with an agent
// in this process at the other end: it answers the host's hello, then
// hands each session to the test (sessions).
type agentBox struct {
	*backendtest.Fake
	sessions chan *agentproto.Session
	mu       sync.Mutex
	dials    int
}

func newAgentBox(status, root string) *agentBox {
	return &agentBox{
		Fake: &backendtest.Fake{Status: status, SandboxLabels: map[string]string{
			assets.LabelRoots: rootsLabel([]config.Root{{Host: root, Container: "/work"}})},
			SandboxMounts: []backend.Mount{{Source: root, Target: "/work"}}},
		sessions: make(chan *agentproto.Session, 16),
	}
}

func (b *agentBox) DialLink() (io.ReadWriteCloser, error) {
	b.mu.Lock()
	b.dials++
	b.mu.Unlock()
	host, agentEnd := net.Pipe()
	go func() {
		s := agentproto.NewSession(agentEnd, agentEnd, false)
		defer s.Close()
		if _, ok := <-s.Control(); !ok {
			return
		}
		if s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version}) != nil {
			return
		}
		select {
		case b.sessions <- s:
		default:
		}
		for range s.Control() {
		}
	}()
	return host, nil
}

func (b *agentBox) dialled() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dials
}

// session is the next session the link opens to b.
func (b *agentBox) session(t *testing.T) *agentproto.Session {
	t.Helper()
	select {
	case s := <-b.sessions:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("the link never connected")
		return nil
	}
}

// linksWorld is a CABOOSE_HOME with environments, each with its sandbox,
// and the daemon's links over them.
type linksWorld struct {
	t      *testing.T
	home   string
	root   string // the host directory of every sandbox's root
	getenv config.Env
	boxes  map[string]*agentBox
	docker string // the fake docker every App of the world runs
}

func newLinksWorld(t *testing.T) *linksWorld {
	home := t.TempDir()
	w := &linksWorld{t: t, home: home, root: t.TempDir(), boxes: map[string]*agentBox{}}
	// A docker whose engine never answers, so that nothing of the daemon's
	// reaches whatever docker this machine has.
	w.docker = filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(w.docker, []byte("#!/bin/sh\necho 'Cannot connect to the Docker daemon' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"HOME": home, "CABOOSE_HOME": home}
	w.getenv = func(k string) string { return vars[k] }
	return w
}

// env makes environment name, with host_exec on, whose sandbox is in
// status.
func (w *linksWorld) env(name, status string) *agentBox {
	w.t.Helper()
	w.writeConfig(name, "3000-3999")
	if err := os.MkdirAll(w.dataDir(name), 0o700); err != nil {
		w.t.Fatal(err)
	}
	b := newAgentBox(status, w.root)
	w.boxes[name] = b
	return b
}

func (w *linksWorld) writeConfig(name, ports string) {
	w.t.Helper()
	dir := config.EnvDirFor(w.home, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		w.t.Fatal(err)
	}
	body := fmt.Sprintf("[link]\nforward_ports = [%q]\nhost_exec = true\nssh_agent = \"none\"\n", ports)
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

func (w *linksWorld) dataDir(name string) string {
	return filepath.Join(config.EnvDirFor(w.home, name), "data")
}

func (w *linksWorld) newApp(env string) (*App, error) {
	app, err := DaemonApp(w.getenv, "")(env)
	if err != nil {
		return nil, err
	}
	app.Docker = &docker.CLI{Path: w.docker, Stderr: io.Discard}
	if b := w.boxes[env]; b != nil {
		app.Backend = b
	} else {
		app.Backend = &backendtest.Fake{}
	}
	return app, nil
}

// links are the daemon's links in daemon directory dir, closed at the
// test's end.
func (w *linksWorld) links(dir string) *DaemonLinks {
	l := NewDaemonLinks(DaemonLinksOptions{Dir: dir, CabooseHome: w.home, NewApp: w.newApp})
	w.t.Cleanup(l.Close)
	return l
}

func (w *linksWorld) daemonDir() string { return filepath.Join(w.home, "daemon") }

func serve(t *testing.T, l *DaemonLinks, method, env string) *daemonproto.LinkResult {
	t.Helper()
	r, err := l.Serve(t.Context(), method, env)
	if err != nil {
		t.Fatalf("%s %s: %v", method, env, err)
	}
	return r
}

func held(t *testing.T, dataDir string) bool {
	t.Helper()
	h, err := linkRunning(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// hostCommand has the link behind s run script on this machine, through
// /bin/sh, and returns its PID once the script has said it ($$ on its
// first line). The process group is killed at the test's end, should it
// still be there.
func hostCommand(t *testing.T, s *agentproto.Session, id uint64, script string) int {
	t.Helper()
	if err := s.Send(agentproto.Message{Type: agentproto.TypeRequest, ID: id, Op: agentproto.OpHostExec}); err != nil {
		t.Fatal(err)
	}
	st, err := s.Accept()
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	var said strings.Builder
	go func() {
		code, err := agentproto.Exec(st, agentproto.ExecRequest{Argv: []string{"/bin/sh", "-c", script}, Dir: "/work"},
			agentproto.ExecIO{Stdout: pw, Stderr: &said})
		pw.CloseWithError(fmt.Errorf("exit %d, %v: %s", code, err, said.String()))
	}()
	line, err := bufio.NewReader(pr).ReadString('\n')
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	pid, perr := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || perr != nil || pid <= 1 {
		t.Fatalf("the host command said %q (%v)", line, err)
	}
	t.Cleanup(func() {
		if syscall.Kill(pid, 0) == nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
	return pid
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// Each environment gets a link of its own, side by side, holding its own
// link.lock, with a link.json that names the daemon and no PID.
func TestDaemonLinksTwoEnvironments(t *testing.T) {
	w := newLinksWorld(t)
	a, b := w.env("alpha", "running"), w.env("beta", "running")
	l := w.links(w.daemonDir())
	for _, env := range []string{"alpha", "beta"} {
		if r := serve(t, l, daemonproto.MethodEnsureLink, env); r.Action != daemonproto.ActionStarted || r.Env.Link != daemonproto.LinkRunning {
			t.Fatalf("ensure %s: %+v", env, r)
		}
	}
	a.session(t)
	b.session(t)
	for _, env := range []string{"alpha", "beta"} {
		if !held(t, w.dataDir(env)) {
			t.Errorf("%s: link.lock is free", env)
		}
		raw, _ := os.ReadFile(filepath.Join(w.dataDir(env), linkStateFile))
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil || m["daemon"] != w.daemonDir() || m["pid"] != nil {
			t.Errorf("%s: link.json %s", env, raw)
		}
	}
	if !l.Busy() {
		t.Error("not busy with two links up")
	}
	envs := l.Envs()
	if len(envs) != 2 || envs[0].Name != "alpha" || envs[1].Name != "beta" ||
		envs[0].Link != daemonproto.LinkRunning || envs[1].Sandbox != daemonproto.SandboxRunning {
		t.Fatalf("envs %+v", envs)
	}
	if r := serve(t, l, daemonproto.MethodEnsureLink, "alpha"); r.Action != daemonproto.ActionUnchanged {
		t.Fatalf("ensure again: %+v", r)
	}
	if r := serve(t, l, daemonproto.MethodStopLink, "alpha"); r.Action != daemonproto.ActionStopped || r.Env.Link != daemonproto.LinkStopped {
		t.Fatalf("stop: %+v", r)
	}
	if held(t, w.dataDir("alpha")) || !held(t, w.dataDir("beta")) {
		t.Fatal("stopping alpha's link: alpha's lock held, or beta's free")
	}
	l.Close()
	if held(t, w.dataDir("beta")) || l.Busy() {
		t.Fatal("Close left beta's link up")
	}
	if _, err := l.Serve(t.Context(), daemonproto.MethodEnsureLink, "beta"); err == nil {
		t.Fatal("a link started after Close")
	}
}

// A sandbox that does not run gets no link; an environment that does not
// exist, or a bad name, is refused.
func TestDaemonLinksNoSandbox(t *testing.T) {
	w := newLinksWorld(t)
	b := w.env("alpha", "exited")
	l := w.links(w.daemonDir())
	if r := serve(t, l, daemonproto.MethodEnsureLink, "alpha"); r.Action != daemonproto.ActionNoSandbox || r.Env.Sandbox != daemonproto.SandboxStopped {
		t.Fatalf("ensure: %+v", r)
	}
	if held(t, w.dataDir("alpha")) || b.dialled() != 0 || l.Busy() {
		t.Fatal("a link runs for a stopped sandbox")
	}
	if _, err := l.Serve(t.Context(), daemonproto.MethodEnsureLink, "nowhere"); daemonproto.Kind(err) != daemonproto.KindUnknownEnv {
		t.Errorf("a missing environment: %v", err)
	}
	if _, err := l.Serve(t.Context(), daemonproto.MethodEnsureLink, "../x"); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("a bad name: %v", err)
	}
}

// A config.toml changed since the link started -- one ensure_link sees
// before the link rereads it -- replaces the link; restart_link replaces
// it whatever.
func TestDaemonLinksEnsureReplacesOnNewSettings(t *testing.T) {
	old := linkConfigEvery
	linkConfigEvery = time.Hour
	t.Cleanup(func() { linkConfigEvery = old })
	w := newLinksWorld(t)
	b := w.env("alpha", "running")
	l := w.links(w.daemonDir())
	serve(t, l, daemonproto.MethodEnsureLink, "alpha")
	first := b.session(t)
	w.writeConfig("alpha", "4000-4100")
	if r := serve(t, l, daemonproto.MethodEnsureLink, "alpha"); r.Action != daemonproto.ActionReplaced || r.Env.Link != daemonproto.LinkRunning {
		t.Fatalf("ensure after the edit: %+v", r)
	}
	select {
	case <-first.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the old link's session goes on")
	}
	b.session(t)
	if st, ok := readLinkState(w.dataDir("alpha")); !ok || st.ForwardPorts != "4000-4100" {
		t.Fatalf("link.json %+v", st)
	}
	if r := serve(t, l, daemonproto.MethodEnsureLink, "alpha"); r.Action != daemonproto.ActionUnchanged {
		t.Fatalf("ensure with the same settings: %+v", r)
	}
	if r := serve(t, l, daemonproto.MethodRestartLink, "alpha"); r.Action != daemonproto.ActionReplaced {
		t.Fatalf("restart: %+v", r)
	}
	b.session(t)
}

// Stopping one environment's link kills the host commands it runs, and
// no other link's.
func TestDaemonLinksStopKillsOnlyItsHostCommands(t *testing.T) {
	w := newLinksWorld(t)
	a, b := w.env("alpha", "running"), w.env("beta", "running")
	l := w.links(w.daemonDir())
	serve(t, l, daemonproto.MethodEnsureLink, "alpha")
	serve(t, l, daemonproto.MethodEnsureLink, "beta")
	pa := hostCommand(t, a.session(t), 1, "echo $$; exec sleep 60")
	pb := hostCommand(t, b.session(t), 1, "echo $$; exec sleep 60")
	if l.HostExecs() != 2 {
		t.Fatalf("%d host commands running, want 2", l.HostExecs())
	}
	serve(t, l, daemonproto.MethodStopLink, "alpha")
	if alive(pa) {
		t.Error("alpha's command outlived its link")
	}
	if !alive(pb) {
		t.Fatal("stopping alpha's link killed beta's command")
	}
	if l.HostExecs() != 1 {
		t.Errorf("%d host commands running, want 1", l.HostExecs())
	}
	l.Close()
	if alive(pb) {
		t.Error("beta's command outlived Close")
	}
}

// A stopped link lets go of link.lock only once what it ran has ended:
// here a host command that ignores SIGTERM, and goes only at the SIGKILL
// that follows it.
func TestDaemonLinksReleaseTheLockLast(t *testing.T) {
	w := newLinksWorld(t)
	a := w.env("alpha", "running")
	l := w.links(w.daemonDir())
	serve(t, l, daemonproto.MethodEnsureLink, "alpha")
	pid := hostCommand(t, a.session(t), 1, `trap "" TERM; echo $$; sleep 60`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = l.Serve(t.Context(), daemonproto.MethodStopLink, "alpha")
	}()
	for {
		select {
		case <-done:
			if alive(pid) || held(t, w.dataDir("alpha")) {
				t.Fatalf("after the stop: command alive %v, lock held %v", alive(pid), held(t, w.dataDir("alpha")))
			}
			return
		case <-time.After(20 * time.Millisecond):
		}
		if !held(t, w.dataDir("alpha")) && alive(pid) {
			t.Fatal("link.lock let go of while the link's command still runs")
		}
	}
}

// heldLockEnv is the variable a re-exec'd test binary takes link.lock in
// the data dir it names under, as a detached helper does.
const heldLockEnv = "CABOOSE_TEST_HOLD_LINK_LOCK"

// holdLinkLock is that binary: it holds the lock until it is killed.
func holdLinkLock(dataDir string) {
	if _, err := lockLink(dataDir); err != nil {
		fmt.Println("cannot lock:", err)
		os.Exit(1)
	}
	fmt.Println("held")
	select {}
}

// detachedHelper starts a process holding dataDir's link.lock, with a
// link.json naming its PID and no daemon, and returns a channel closed
// once it has exited.
func detachedHelper(t *testing.T, dataDir string) (pid int, exited <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), heldLockEnv+"="+dataDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	gone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(gone)
	}()
	t.Cleanup(func() {
		select {
		case <-gone:
		default:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-gone
		}
	})
	line, _ := bufio.NewReader(out).ReadString('\n')
	if line != "held\n" {
		t.Fatalf("the helper said %q", line)
	}
	if err := writeLinkState(dataDir, linkState{PID: cmd.Process.Pid, linkSettings: settingsFor("3000", "ask")}); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid, gone
}

// A detached helper holding link.lock is left alone: the daemon's link
// fails, saying who holds it, and nothing is signalled. So is a link
// another daemon serves.
func TestDaemonLinksLeaveAHeldLockAlone(t *testing.T) {
	w := newLinksWorld(t)
	w.env("alpha", "running")
	_, exited := detachedHelper(t, w.dataDir("alpha"))
	signalled := false
	orig := linkKill
	linkKill = func(int, syscall.Signal) error { signalled = true; return nil }
	defer func() { linkKill = orig }()
	l := w.links(w.daemonDir())
	_, err := l.Serve(t.Context(), daemonproto.MethodEnsureLink, "alpha")
	if err == nil || !strings.Contains(err.Error(), "caboose link --background") || signalled {
		t.Fatalf("a detached helper's link: err %v, signalled %v", err, signalled)
	}
	select {
	case <-exited:
		t.Fatal("the detached helper was stopped")
	case <-time.After(200 * time.Millisecond):
	}
	if st, ok := readLinkState(w.dataDir("alpha")); !ok || st.Daemon != "" || st.PID == 0 {
		t.Fatalf("link.json %+v", st)
	}
	// stop_link leaves the helper's link.json as it is.
	if r := serve(t, l, daemonproto.MethodStopLink, "alpha"); r.Action != daemonproto.ActionStopped {
		t.Fatalf("stop: %+v", r)
	}
	if st, _ := readLinkState(w.dataDir("alpha")); st.Stopped {
		t.Fatalf("stop_link marked a helper's link.json: %+v", st)
	}

	// Another daemon's: its lock held, its link.json naming it.
	w.env("beta", "running")
	unlock, err := lockLink(w.dataDir("beta"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := writeLinkState(w.dataDir("beta"), linkState{PID: os.Getpid(), Daemon: filepath.Join(w.home, "daemon-dev")}); err != nil {
		t.Fatal(err)
	}
	_, err = l.Serve(t.Context(), daemonproto.MethodEnsureLink, "beta")
	if err == nil || !strings.Contains(err.Error(), "daemon-dev") || signalled {
		t.Fatalf("another daemon's link: err %v, signalled %v", err, signalled)
	}
	if st := l.Envs(); len(st) != 2 || st[1].Link != daemonproto.LinkFailed || st[1].LastError == "" {
		t.Fatalf("status %+v", st)
	}
}

// At start the daemon takes back the links of environments whose
// link.json names it, and whose sandbox runs; never one naming no daemon
// (a detached helper's), whichever daemon it is.
func TestDaemonLinksRecover(t *testing.T) {
	for _, name := range []string{"daemon", "daemon-dev"} {
		t.Run(name, func(t *testing.T) {
			w := newLinksWorld(t)
			dir := filepath.Join(w.home, name)
			boxes := map[string]*agentBox{
				"mine":    w.env("mine", "running"),
				"other":   w.env("other", "running"),
				"stopped": w.env("stopped", "exited"),
				"helper":  w.env("helper", "running"),
				"fresh":   w.env("fresh", "running"),
			}
			state := func(env, daemon string) {
				if err := writeLinkState(w.dataDir(env), linkState{Daemon: daemon, linkSettings: settingsFor("3000", "ask")}); err != nil {
					t.Fatal(err)
				}
			}
			state("mine", dir)
			state("other", filepath.Join(t.TempDir(), "daemon"))
			state("stopped", dir)
			state("helper", "")
			// A directory that is no environment's name is never one.
			if err := os.MkdirAll(filepath.Join(w.home, "envs", "Not.An.Env", "data"), 0o700); err != nil {
				t.Fatal(err)
			}
			l := w.links(dir)
			l.Recover(nil)
			want := map[string]bool{"mine": true}
			for env, b := range boxes {
				if up := held(t, w.dataDir(env)); up != want[env] {
					t.Errorf("%s: link up %v, want %v", env, up, want[env])
				}
				if want[env] {
					b.session(t)
				} else if b.dialled() != 0 {
					t.Errorf("%s: dialled", env)
				}
			}
		})
	}
}

// Host commands are counted and killed per link, and every link's at once.
func TestDaemonLinksHostExecCount(t *testing.T) {
	w := newLinksWorld(t)
	a := w.env("alpha", "running")
	l := w.links(w.daemonDir())
	if l.HostExecs() != 0 {
		t.Fatalf("%d running at the start", l.HostExecs())
	}
	serve(t, l, daemonproto.MethodEnsureLink, "alpha")
	s := a.session(t)
	pids := []int{hostCommand(t, s, 1, "echo $$; exec sleep 60"), hostCommand(t, s, 2, "echo $$; exec sleep 60")}
	if n := l.HostExecs(); n != 2 {
		t.Fatalf("%d running, want 2", n)
	}
	l.Close()
	for _, p := range pids {
		if alive(p) {
			t.Errorf("%d outlived Close", p)
		}
	}
	if n := l.HostExecs(); n != 0 {
		t.Errorf("%d running after Close", n)
	}
}

// The daemon's links read their configuration without the variables that
// are a command's.
func TestDaemonGetenv(t *testing.T) {
	env := map[string]string{"CABOOSE_DATA_DIR": "/d", "CABOOSE_ENV": "e", "CABOOSE_SESSION": "s", "HOME": "/h", "CABOOSE_HOME": "/c"}
	g := DaemonGetenv(func(k string) string { return env[k] })
	for k, want := range map[string]string{"CABOOSE_DATA_DIR": "", "CABOOSE_ENV": "", "CABOOSE_SESSION": "", "HOME": "/h", "CABOOSE_HOME": "/c"} {
		if got := g(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// stopLink never signals a link a daemon serves, even with a live PID in
// its link.json, nor this process.
func TestStopLinkLeavesADaemonsLinkAlone(t *testing.T) {
	child := exec.Command("sleep", "60")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		_ = child.Wait()
	})
	for name, st := range map[string]linkState{
		"a daemon's":                  {Daemon: "/x/daemon"},
		"a daemon's, with a live pid": {Daemon: "/x/daemon", PID: child.Process.Pid},
		"this process":                {PID: os.Getpid()},
	} {
		dir := t.TempDir()
		unlock, err := lockLink(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeLinkState(dir, st); err != nil {
			t.Fatal(err)
		}
		signalled := false
		orig := linkKill
		linkKill = func(int, syscall.Signal) error { signalled = true; return nil }
		err = stopLink(dir)
		linkKill = orig
		unlock()
		if err == nil || signalled {
			t.Errorf("%s: err %v, signalled %v", name, err, signalled)
		}
	}
}

// A link.json left by a helper that is gone -- link.lock free -- names a
// PID that may be another process's by now: it is never signalled.
func TestStopLinkSignalsOnlyWhileTheLockIsHeld(t *testing.T) {
	child := exec.Command("sleep", "60")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		_ = child.Wait()
	})
	dir := t.TempDir()
	if err := writeLinkState(dir, linkState{PID: child.Process.Pid, linkSettings: settingsFor("3000", "ask")}); err != nil {
		t.Fatal(err)
	}
	signalled := false
	orig := linkKill
	linkKill = func(int, syscall.Signal) error { signalled = true; return nil }
	defer func() { linkKill = orig }()
	if err := stopLink(dir); err != nil || signalled {
		t.Fatalf("a stale link.json: err %v, signalled %v", err, signalled)
	}
}

// A link stopped with stop_link stays stopped when the daemon starts again
// (recovery), until ensure_link starts it, which clears the mark.
func TestDaemonLinksStopIsNotRecovered(t *testing.T) {
	w := newLinksWorld(t)
	a := w.env("alpha", "running")
	l := w.links(w.daemonDir())
	serve(t, l, daemonproto.MethodEnsureLink, "alpha")
	a.session(t)
	serve(t, l, daemonproto.MethodStopLink, "alpha")
	st, ok := readLinkState(w.dataDir("alpha"))
	if !ok || !st.Stopped || st.Daemon != w.daemonDir() {
		t.Fatalf("link.json after stop_link: %+v", st)
	}
	l.Close()

	again := w.links(w.daemonDir())
	again.Recover(nil)
	if held(t, w.dataDir("alpha")) || a.dialled() != 1 {
		t.Fatal("recovery brought back a link stopped on purpose")
	}
	if r := serve(t, again, daemonproto.MethodEnsureLink, "alpha"); r.Action != daemonproto.ActionStarted {
		t.Fatalf("ensure: %+v", r)
	}
	a.session(t)
	if st, ok := readLinkState(w.dataDir("alpha")); !ok || st.Stopped {
		t.Fatalf("link.json after ensure_link: %+v", st)
	}
}

// The daemon serves every environment at its own default data dir, whatever
// CABOOSE_DATA_DIR its process was somehow given: one data dir for all
// would be one link.lock for all.
func TestDaemonAppIgnoresDataDirVariable(t *testing.T) {
	w := newLinksWorld(t)
	w.env("one", "running")
	w.env("two", "running")
	shared := t.TempDir()
	getenv := DaemonGetenv(func(k string) string {
		if k == "CABOOSE_DATA_DIR" {
			return shared
		}
		return w.getenv(k)
	})
	var dirs []string
	for _, e := range []string{"one", "two"} {
		app, err := DaemonApp(getenv, "")(e)
		if err != nil {
			t.Fatal(err)
		}
		if app.Cfg.DataDir != w.dataDir(e) {
			t.Errorf("%s: data dir %q, want %q", e, app.Cfg.DataDir, w.dataDir(e))
		}
		dirs = append(dirs, app.Cfg.DataDir)
	}
	if dirs[0] == dirs[1] {
		t.Error("two environments share a data dir")
	}
}

func TestDaemonServesOnlyDefaultDataDirs(t *testing.T) {
	for _, tc := range []struct {
		dataDir string
		want    bool
	}{{"", true}, {"/some/dir", false}} {
		got := DaemonServes(func(k string) string {
			if k == "CABOOSE_DATA_DIR" {
				return tc.dataDir
			}
			return ""
		})
		if got != tc.want {
			t.Errorf("CABOOSE_DATA_DIR=%q: %v, want %v", tc.dataDir, got, tc.want)
		}
	}
}

// The daemon's links admit host commands through its gate: a drain fails
// while one runs, and succeeds once it has ended.
func TestDaemonLinksDrainWaitsForHostCommands(t *testing.T) {
	w := newLinksWorld(t)
	a := w.env("alpha", "running")
	l := w.links(w.daemonDir())
	serve(t, l, daemonproto.MethodEnsureLink, "alpha")
	s := a.session(t)
	pid := hostCommand(t, s, 1, "echo $$; exec sleep 60")
	if l.DrainHostExecs(true) {
		t.Fatal("drained while a host command runs")
	}
	if n := l.gate.InFlight(); n != 1 {
		t.Fatalf("%d host requests in flight, want 1", n)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	deadline := time.Now().Add(5 * time.Second)
	for !l.DrainHostExecs(false) {
		if time.Now().After(deadline) {
			t.Fatal("no drain after the host command ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Recovering holds from WillRecover until Recover has looked at every
// environment, so that status can tell a link on its way back from one
// the daemon does not serve.
func TestDaemonLinksRecovering(t *testing.T) {
	w := newLinksWorld(t)
	b := w.env("alpha", "running")
	if err := writeLinkState(w.dataDir("alpha"), linkState{Daemon: w.daemonDir(), linkSettings: settingsFor("3000-3999", "ask")}); err != nil {
		t.Fatal(err)
	}
	l := w.links(w.daemonDir())
	if l.Recovering() {
		t.Fatal("recovering before WillRecover")
	}
	l.WillRecover()
	if !l.Recovering() {
		t.Fatal("not recovering after WillRecover")
	}
	l.Recover(nil)
	if l.Recovering() {
		t.Fatal("still recovering after Recover")
	}
	b.session(t)
	if envs := l.Envs(); len(envs) != 1 || envs[0].Link != daemonproto.LinkRunning {
		t.Fatalf("envs after recovery: %+v", envs)
	}
}
