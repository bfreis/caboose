package launcher

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/vm"
)

func TestLinkLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	if held, err := linkRunning(dir); err != nil || held {
		t.Fatalf("fresh dir: held %v, err %v", held, err)
	}
	unlock, err := lockLink(dir)
	if err != nil {
		t.Fatal(err)
	}
	if held, _ := linkRunning(dir); !held {
		t.Fatal("a held lock reads as free")
	}
	if _, err := lockLink(dir); err == nil {
		t.Fatal("locked twice")
	}
	unlock()
	if held, _ := linkRunning(dir); held {
		t.Fatal("a released lock reads as held")
	}
}

func TestLinkRefusesBadSettings(t *testing.T) {
	cases := map[string]config.Config{
		"forward_ports": {ForwardPorts: "3000-", OpenURLs: "ask"},
		"open_urls":     {ForwardPorts: "3000", OpenURLs: "sometimes"},
		"egress_ports":  {ForwardPorts: "3000", OpenURLs: "ask", EgressPorts: "443 70000"},
		"egress_allow":  {ForwardPorts: "3000", OpenURLs: "ask", EgressAllow: []string{"10.0.0.0/33"}},
	}
	for want, cfg := range cases {
		cfg.DataDir = t.TempDir()
		var stderr bytes.Buffer
		a := &App{Cfg: &cfg, Stderr: &stderr}
		err := a.Link(nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	a := &App{Cfg: &config.Config{ForwardPorts: "3000", OpenURLs: "ask", DataDir: t.TempDir()}}
	if err := a.Link([]string{"--now"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("a stray argument: %v", err)
	}
}

// A launch starts no second helper while one holds the lock.
func TestStartLinkOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	var spawned [][]string
	a := &App{
		Cfg:        &config.Config{Env: "default", DataDir: dir},
		Executable: func() (string, error) { return "/bin/caboose", nil },
		Spawn: func(exe string, args ...string) error {
			spawned = append(spawned, append([]string{exe}, args...))
			return nil
		},
	}
	a.startLink()
	if len(spawned) != 1 || strings.Join(spawned[0], " ") != "/bin/caboose --env default link --background" {
		t.Fatalf("spawned %v", spawned)
	}
	unlock, err := lockLink(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	a.startLink()
	if len(spawned) != 1 {
		t.Fatalf("spawned again while a link runs: %v", spawned)
	}
}

// fakeHelper holds the link lock as a running helper would, with state
// saying it runs with settings; linkKill "stops" it by releasing the lock.
func fakeHelper(t *testing.T, dir string, settings linkSettings) (killed *[]int) {
	t.Helper()
	unlock, err := lockLink(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLinkState(dir, linkState{PID: 4242, linkSettings: settings}); err != nil {
		t.Fatal(err)
	}
	var k []int
	orig := linkKill
	linkKill = func(pid int, sig syscall.Signal) error {
		k = append(k, pid)
		unlock()
		return nil
	}
	t.Cleanup(func() { linkKill = orig; unlock() })
	return &k
}

func spawnRecorder(a *App) *[][]string {
	var spawned [][]string
	a.Executable = func() (string, error) { return "/bin/caboose", nil }
	a.Spawn = func(exe string, args ...string) error {
		spawned = append(spawned, append([]string{exe}, args...))
		return nil
	}
	return &spawned
}

// A launch whose settings differ from the running helper's -- a variable
// set in that shell, say -- replaces it; one with the same leaves it be.
func TestStartLinkReplacesAHelperWithOtherSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		running  linkSettings
		replaced bool
	}{
		{"same settings", settingsFor("3000", "ask"), false},
		{"other ports", settingsFor("4000", "ask"), true},
		{"other open_urls", settingsFor("3000", "off"), true},
		// The container and the VM share the environment's name, and the
		// old one may still run: a helper serves the one it started for.
		{"other isolation", settingsFor("3000", "ask").withIsolation(isolationVM), true},
		// ssh_agent set since: the VM is to get another agent.
		{"other ssh_agent", func() linkSettings { s := settingsFor("3000", "ask"); s.SSHAgent = "/op.sock"; return s }(), true},
		// host_exec turned on in config.toml, which the helper has not read:
		// the launch replaces it, so it stops offering it.
		{"other host_exec", func() linkSettings { s := settingsFor("3000", "ask"); s.HostExec = true; return s }(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			killed := fakeHelper(t, dir, tc.running)
			a := &App{Cfg: &config.Config{Env: "default", DataDir: dir, ForwardPorts: "3000", OpenURLs: "ask", Egress: true}}
			spawned := spawnRecorder(a)
			a.startLink()
			if got := len(*spawned) == 1 && len(*killed) == 1 && (*killed)[0] == 4242; got != tc.replaced {
				t.Fatalf("killed %v, spawned %v; want replaced %v", *killed, *spawned, tc.replaced)
			}
		})
	}
}

func TestRestartLink(t *testing.T) {
	dir := t.TempDir()
	killed := fakeHelper(t, dir, settingsFor("3000", "ask"))
	var stderr bytes.Buffer
	a := &App{Cfg: &config.Config{Env: "default", DataDir: dir, Container: "c", ForwardPorts: "3000", OpenURLs: "ask", Egress: true},
		Docker: &docker.CLI{Path: "/nonexistent/docker"}, Stderr: &stderr}
	spawned := spawnRecorder(a)
	if err := a.Link([]string{"--restart"}); err != nil {
		t.Fatal(err)
	}
	if len(*killed) != 1 || len(*spawned) != 1 || !strings.Contains(stderr.String(), "stopped the running link") {
		t.Fatalf("killed %v, spawned %v, said %q", *killed, *spawned, stderr.String())
	}
	// With nothing running, it only starts one.
	*spawned = nil
	stderr.Reset()
	if err := a.Link([]string{"--restart"}); err != nil {
		t.Fatal(err)
	}
	if len(*spawned) != 1 || strings.Contains(stderr.String(), "stopped") {
		t.Fatalf("spawned %v, said %q", *spawned, stderr.String())
	}
}

// An edit to config.toml reaches the running helper: new settings end the
// session, which reconnects with them; a broken edit keeps the old ones.
func TestReloadOnConfigChange(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home, "CABOOSE_HOME": home}
	getenv := func(k string) string { return env[k] }
	cfg, err := config.Load(getenv, config.OSFS{}, "")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.EnvDir, 0o755)
	os.MkdirAll(cfg.DataDir, 0o755)
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(cfg.EnvDir, config.FileName), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := &linkRunner{a: &App{Cfg: cfg}, log: log.New(io.Discard, "", 0), settings: settingsOf(cfg)}
	sess := agentproto.NewSession(bytes.NewReader(nil), nopCloser{io.Discard}, true)
	r.sess = sess

	write("[link]\nforward_ports = [\"4000-4100\"]\nopen_urls = \"off\"\n")
	r.reload()
	if r.settings != (settingsFor("4000-4100", "off")) || !r.cfg.Ports.Has(4050) || r.cfg.OpenURL != "off" {
		t.Fatalf("settings %+v after the edit", r.settings)
	}
	select {
	case <-sess.Done():
	default:
		t.Fatal("the session goes on with the old settings")
	}
	if st, ok := readLinkState(cfg.DataDir); !ok || st.linkSettings != r.settings {
		t.Fatalf("link.json %+v", st)
	}

	write("[link]\nforward_ports = [\"4000-\"]\n")
	r.reload()
	if r.settings != (settingsFor("4000-4100", "off")) {
		t.Fatalf("a broken edit replaced the settings: %+v", r.settings)
	}
}

// A helper that sees config.toml change the isolation retires: it cannot
// serve the new sandbox, and must not stay on the old one, which may still
// run under the same name. Its session ends, its settings stay, and run
// returns at once, so the next launch starts a helper for the new one.
func TestReloadRetiresOnIsolationChange(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string { return map[string]string{"HOME": home, "CABOOSE_HOME": home}[k] }
	cfg, err := config.Load(getenv, config.OSFS{}, "")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.EnvDir, 0o755)
	os.MkdirAll(cfg.DataDir, 0o755)
	r := &linkRunner{a: &App{Cfg: cfg}, log: log.New(io.Discard, "", 0), settings: settingsOf(cfg)}
	sess := agentproto.NewSession(bytes.NewReader(nil), nopCloser{io.Discard}, true)
	r.sess = sess
	before := r.settings

	if err := os.WriteFile(filepath.Join(cfg.EnvDir, config.FileName), []byte("isolation = \"vm.default\"\n[vm.default]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.reload()
	select {
	case <-sess.Done():
	default:
		t.Fatal("the session goes on with the old sandbox")
	}
	if !r.retired || r.settings != before {
		t.Fatalf("retired %v, settings %+v; want retired, settings as they were", r.retired, r.settings)
	}
	done := make(chan error, 1)
	go func() { done <- r.run(true) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a retired helper goes on running")
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// linkerBox is a sandbox whose link is a port to dial, as a VM's is; any
// command run in it fails the test.
type linkerBox struct {
	backend.Backend
	t    *testing.T
	dial func() (io.ReadWriteCloser, error)
}

func (b linkerBox) DialLink() (io.ReadWriteCloser, error) { return b.dial() }

func (b linkerBox) Labels() (map[string]string, error) { return map[string]string{}, nil }

func (b linkerBox) Command(s backend.ExecSpec) *exec.Cmd {
	b.t.Errorf("ran %q: a dialled link runs nothing", s.Argv)
	return exec.Command("false")
}

// A sandbox that is a Linker is linked by dialling it, and an agent of
// another version there stops the link, as it does over docker exec.
func TestLinkDialsALinker(t *testing.T) {
	host, agentEnd := net.Pipe()
	go func() {
		s := agentproto.NewSession(agentEnd, agentEnd, false)
		_ = s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version + 1})
		<-s.Done()
	}()
	dialled := 0
	box := linkerBox{t: t, dial: func() (io.ReadWriteCloser, error) { dialled++; return host, nil }}
	cfg := &config.Config{Container: "box", DataDir: t.TempDir(), Getenv: func(string) string { return "" }}
	r := &linkRunner{a: &App{Cfg: cfg, Backend: box}, log: log.New(io.Discard, "", 0)}
	err := r.once()
	var stop *linkStop
	if !errors.As(err, &stop) || !strings.Contains(stop.msg, "another protocol version") {
		t.Fatalf("err = %v", err)
	}
	if dialled != 1 {
		t.Fatalf("dialled %d times", dialled)
	}
}

// settingsFor is linkSettings with these two, the outbound proxy's
// defaults, and the default isolation.
func settingsFor(forwardPorts, openURLs string) linkSettings {
	return linkSettings{ForwardPorts: forwardPorts, OpenURLs: openURLs,
		Egress: true, EgressPorts: config.DefaultEgressPorts, Isolation: isolationContainer}
}

// withIsolation is s serving a sandbox of another isolation.
func (s linkSettings) withIsolation(iso string) linkSettings { s.Isolation = iso; return s }

// A vm guest's link offers the outbound proxy in the host's hello, unless
// egress is off.
func TestLinkOffersEgressToALinker(t *testing.T) {
	for egress, want := range map[bool]string{true: agentproto.EgressListen, false: ""} {
		lc, err := settingsFor("3000", "ask").withEgress(egress).check()
		if err != nil {
			t.Fatal(err)
		}
		host, agentEnd := net.Pipe()
		offered := make(chan string, 1)
		go func() {
			s := agentproto.NewSession(agentEnd, agentEnd, false)
			b := <-s.Control()
			m, _ := agentproto.Decode(b)
			offered <- m.Egress
			_ = s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version + 1})
			<-s.Done()
		}()
		box := linkerBox{t: t, dial: func() (io.ReadWriteCloser, error) { return host, nil }}
		cfg := &config.Config{Container: "box", DataDir: t.TempDir(), Getenv: func(string) string { return "" }}
		r := &linkRunner{a: &App{Cfg: cfg, Backend: box}, log: log.New(io.Discard, "", 0),
			cfg: hostlink.Config{Ports: lc.ports, Egress: lc.egress}}
		_ = r.once()
		if got := <-offered; got != want {
			t.Errorf("egress %v: offered %q, want %q", egress, got, want)
		}
	}
}

func (s linkSettings) withEgress(on bool) linkSettings { s.Egress = on; return s }

// link.log is appended to across links, and moved aside to link.log.1,
// saying so, rather than grow past its cap.
func TestCappedLinkLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), linkLogFile)
	if err := os.WriteFile(path, []byte("from the last link\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := openCappedLog(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, "from this one\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "from the last link\nfrom this one\n" {
		t.Fatalf("a new link's log: %q", b)
	}
	line := strings.Repeat("x", 99) + "\n"
	for i := 0; i < 30000; i++ {
		if _, err := io.WriteString(c, line); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	for _, p := range []string{path, path + ".1"} {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() > 1<<20 {
			t.Fatalf("%s: %v", p, err)
		}
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "link.log reached 1 MiB: the lines before are in link.log.1\n") || !strings.HasSuffix(string(b), line) {
		t.Errorf("link.log after moving aside:\n%.200s", b)
	}
	old, _ := os.ReadFile(path + ".1")
	if !strings.HasSuffix(string(old), line) {
		t.Errorf("link.log.1:\n%.200s", old)
	}
}

// A docker exec of caboose-agent that cannot run it (126, 127) means the
// container's image has no agent: the link stops and says to restart,
// rather than retry. The agent exiting any other way is not that. This is
// docker's and gvisor's path alone: a VM's link is dialled
// (TestLinkVMIsDialled), with no exec to fail.
func TestLinkExecWithoutAgentSaysRestart(t *testing.T) {
	for _, code := range []int{126, 127, 1, 2} {
		box := &backendtest.Fake{Status: "running", Exec: func(backend.ExecSpec) *exec.Cmd {
			return backendtest.Reply("", "exec: no such file", code)
		}}
		cfg := &config.Config{Container: "box", DataDir: t.TempDir(), Getenv: func(string) string { return "" }}
		r := &linkRunner{a: &App{Cfg: cfg, Backend: box}, log: log.New(io.Discard, "", 0)}
		err := r.once()
		if !box.Ran(AgentPath, "link") {
			t.Fatalf("exit %d: ran %v, want %s link", code, box.Execs, AgentPath)
		}
		var stop *linkStop
		missing := errors.As(err, &stop) && strings.Contains(stop.msg, "has no caboose-agent") &&
			strings.Contains(stop.msg, "'caboose restart'") && strings.Contains(stop.msg, "(exec: no such file)")
		if want := code == 126 || code == 127; missing != want {
			t.Errorf("exit %d: err = %v, want the missing-agent stop: %v", code, err, want)
		}
		if code != 126 && code != 127 {
			if errors.As(err, &stop) {
				t.Errorf("exit %d: stopped the link (%v), want it retried", code, err)
			}
			if err == nil || !strings.Contains(err.Error(), "the agent said: exec: no such file") {
				t.Errorf("exit %d: err = %v, want what the agent said", code, err)
			}
		}
	}
}

// A VM's link is the guest's port, never an exec of caboose-agent, so the
// missing-agent stop cannot arise there: a VM whose exec helper would exit
// 127 is dialled instead, and its absent port is a failure to retry.
func TestLinkVMIsDialled(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	self := filepath.Join(dir, "self")
	if err := os.WriteFile(self, []byte("#!/bin/sh\ntouch "+ran+"\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	box := &backend.VM{Name: "box", Dir: vm.Dir(filepath.Join(dir, "vm")), Self: self}
	cfg := &config.Config{Container: "box", DataDir: t.TempDir(), Getenv: func(string) string { return "" }}
	r := &linkRunner{a: &App{Cfg: cfg, Backend: box}, log: log.New(io.Discard, "", 0)}
	err := r.once()
	var stop *linkStop
	if err == nil || errors.As(err, &stop) || !strings.Contains(err.Error(), "vm.sock") {
		t.Fatalf("err = %v, want a failure to dial vm.sock", err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("ran a command in the VM: its link is dialled")
	}
}

// The first session in a row to end before the agent's hello -- a VM's
// agent with no boot to serve yet -- is retried at once, almost, and
// the backoff then goes on from where it was; anything else doubles it.
func TestLinkPause(t *testing.T) {
	noHello := fmt.Errorf("%w (agentproto: session closed)", hostlink.ErrNoHello)
	pause, wait, early := linkPause(noHello, linkRetryMin, false)
	if pause != linkRetryEarly || wait != linkRetryMin || !early {
		t.Errorf("first no-hello: %v, %v, %v", pause, wait, early)
	}
	pause, wait, early = linkPause(noHello, wait, early)
	if pause != linkRetryMin || wait != 2*linkRetryMin || !early {
		t.Errorf("second no-hello: %v, %v, %v", pause, wait, early)
	}
	if pause, wait, early := linkPause(errors.New("broken"), linkRetryMin, false); pause != linkRetryMin || wait != 2*linkRetryMin || early {
		t.Errorf("another ending: %v, %v, %v", pause, wait, early)
	}
	if pause, wait, _ := linkPause(noHello, 4*linkRetryMin, false); pause != 4*linkRetryMin || wait != 8*linkRetryMin {
		t.Errorf("no-hello well into the backoff: %v, %v", pause, wait)
	}
	if pause, wait, _ := linkPause(noHello, linkRetryMax, true); pause != linkRetryMax || wait != linkRetryMax {
		t.Errorf("at the cap: %v, %v", pause, wait)
	}
}

func (b linkerBox) State() string { return "running" }

// A helper that stops for good records why, for a launch to say
// (awaitProxy); a helper starting, and a launch about to start one, forget
// it, so it is never a stale helper's.
func TestLinkRecordsItsStop(t *testing.T) {
	host, agentEnd := net.Pipe()
	go func() {
		s := agentproto.NewSession(agentEnd, agentEnd, false)
		_ = s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version + 1})
		<-s.Done()
	}()
	box := linkerBox{t: t, dial: func() (io.ReadWriteCloser, error) { return host, nil }}
	dir := t.TempDir()
	cfg := &config.Config{Env: "default", Container: "box", DataDir: dir, ForwardPorts: "3000", OpenURLs: "ask", Getenv: func(string) string { return "" }}
	r := &linkRunner{a: &App{Cfg: cfg, Backend: box}, log: log.New(io.Discard, "", 0)}
	if err := r.run(true); err != nil {
		t.Fatal(err)
	}
	msg, ok := readLinkStop(dir)
	if !ok || !strings.Contains(msg, "another protocol version") || !strings.Contains(msg, "'caboose restart'") {
		t.Fatalf("link.stop: %q, %v", msg, ok)
	}

	a := &App{Cfg: cfg}
	spawned := spawnRecorder(a)
	a.startLink()
	if _, ok := readLinkStop(dir); ok || len(*spawned) != 1 {
		t.Fatalf("starting a link: spawned %v, link.stop kept %v", *spawned, ok)
	}

	if err := writeLinkStop(dir, "stopped"); err != nil {
		t.Fatal(err)
	}
	a.Docker, a.Stderr = &docker.CLI{Path: "/nonexistent/docker"}, io.Discard
	if err := a.Link([]string{"--restart"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readLinkStop(dir); ok {
		t.Fatal("caboose link --restart kept link.stop")
	}
}

// host_exec is a link setting: turned on in config.toml, the running
// helper reconnects offering it; a value it does not take keeps the old.
func TestReloadHostExec(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string { return map[string]string{"HOME": home, "CABOOSE_HOME": home}[k] }
	cfg, err := config.Load(getenv, config.OSFS{}, "")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.EnvDir, 0o755)
	os.MkdirAll(cfg.DataDir, 0o755)
	r := &linkRunner{a: &App{Cfg: cfg}, log: log.New(io.Discard, "", 0), settings: settingsOf(cfg)}
	if r.settings.HostExec || r.cfg.HostExec != nil {
		t.Fatalf("on by default: %+v", r.settings)
	}
	sess := agentproto.NewSession(bytes.NewReader(nil), nopCloser{io.Discard}, true)
	r.sess = sess
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(cfg.EnvDir, config.FileName), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("[link]\nhost_exec = true\n")
	r.reload()
	if !r.settings.HostExec || r.cfg.HostExec == nil {
		t.Fatalf("settings %+v, offer %v after turning it on", r.settings, r.cfg.HostExec)
	}
	select {
	case <-sess.Done():
	default:
		t.Fatal("the session goes on without it")
	}
	write("[link]\nhost_exec = \"maybe\"\n")
	r.reload()
	if !r.settings.HostExec || r.cfg.HostExec == nil {
		t.Fatalf("a value it does not take replaced it: %+v", r.settings)
	}
}

// mountsBox is a linkerBox that has roots mounted.
type mountsBox struct {
	linkerBox
	mounts []backend.Mount
}

func (b mountsBox) Mounts() ([]backend.Mount, error) { return b.mounts, nil }

// Labels names the mounts as the roots the sandbox was created with.
func (b mountsBox) Labels() (map[string]string, error) {
	var roots []config.Root
	for _, m := range b.mounts {
		roots = append(roots, config.Root{Host: m.Source, Container: m.Target})
	}
	return map[string]string{assets.LabelRoots: rootsLabel(roots)}, nil
}

// With host_exec on, the hello offers host exec, and a command runs in the
// host directory of the root the sandbox mounts, not a configured one.
func TestLinkOffersHostExec(t *testing.T) {
	mounted := t.TempDir()
	for _, on := range []bool{true, false} {
		lc, err := func() (linkConfig, error) {
			s := settingsFor("3000", "ask")
			if on {
				s.HostExec = true
			}
			return s.check()
		}()
		if err != nil {
			t.Fatal(err)
		}
		host, agentEnd := net.Pipe()
		got := make(chan string, 1)
		go func() {
			s := agentproto.NewSession(agentEnd, agentEnd, false)
			defer s.Close()
			b := <-s.Control()
			m, _ := agentproto.Decode(b)
			if !m.HostExec {
				got <- "not offered"
				return
			}
			_ = s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version})
			_ = s.Send(agentproto.Message{Type: agentproto.TypeRequest, ID: 1, Op: agentproto.OpHostExec})
			st, err := s.Accept()
			if err != nil {
				got <- err.Error()
				return
			}
			var out, errs bytes.Buffer
			code, err := agentproto.Exec(st, agentproto.ExecRequest{Argv: []string{"pwd", "-P"}, Dir: "/work"}, agentproto.ExecIO{Stdout: &out, Stderr: &errs})
			got <- fmt.Sprintf("%d %v %s%s", code, err, strings.TrimSpace(out.String()), errs.String())
		}()
		box := mountsBox{linkerBox{t: t, dial: func() (io.ReadWriteCloser, error) { return host, nil }},
			[]backend.Mount{{Source: mounted, Target: "/work"}}}
		cfg := &config.Config{Container: "box", DataDir: t.TempDir(), Getenv: func(string) string { return "" },
			Roots: []config.Root{{Host: "/configured/elsewhere", Container: "/work"}}}
		r := &linkRunner{a: &App{Cfg: cfg, Backend: box}, log: log.New(io.Discard, "", 0),
			cfg: hostlink.Config{Ports: lc.ports, HostExec: hostExecOffer(lc.hostExec)}}
		go func() { _ = r.once() }()
		phys, _ := filepath.EvalSymlinks(mounted)
		want := "not offered"
		if on {
			want = "0 <nil> " + phys
		}
		select {
		case g := <-got:
			if g != want {
				t.Errorf("host_exec %v: %q, want %q", on, g, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("host_exec %v: nothing came of it", on)
		}
		host.Close()
	}
}
