package launcher

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/saferm"
)

// fakeDaemon stands in for the daemon the commands ask (App.DaemonDial): it
// records what is asked, and answers as it is told.
type fakeDaemon struct {
	mu       sync.Mutex
	ensured  []string // environments asked for ensure_link
	restarts []string
	dials    []bool // each dial's start
	// Dial, when set, is the error of dialing.
	Dial error
	// Err, when set, is the error of every call.
	Err error
	// Result is the answer to a link call; nil is a started link.
	Result *daemonproto.LinkResult
	// Status is the answer to status; nil is "unavailable".
	Stat *daemonproto.StatusResult
	// Stats, when set, are the answers to status in turn, the last one
	// for every call after; Stat answers the dial.
	Stats       []*daemonproto.StatusResult
	statusCalls int
	// WebRes is the answer to web; nil is a listening page.
	WebRes   *daemonproto.WebResult
	webCalls []daemonproto.WebParams
	// OnEnsure runs at each ensure_link.
	OnEnsure func()
	// br is the request broker's side (requests_test.go).
	br brokerFake
}

func (f *fakeDaemon) dial(_ context.Context, start bool) (DaemonCaller, error) {
	f.mu.Lock()
	f.dials = append(f.dials, start)
	f.mu.Unlock()
	if f.Dial != nil {
		return nil, f.Dial
	}
	if f.Stat == nil && !start {
		return nil, daemonproto.NewError(daemonproto.KindUnavailable, "no daemon answers")
	}
	return f, nil
}

func (f *fakeDaemon) result(env string) (*daemonproto.LinkResult, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	if f.Result != nil {
		return f.Result, nil
	}
	return &daemonproto.LinkResult{Action: daemonproto.ActionStarted, Env: daemonproto.EnvStatus{Name: env, Sandbox: daemonproto.SandboxRunning, Link: daemonproto.LinkRunning}}, nil
}

func (f *fakeDaemon) EnsureLink(_ context.Context, env string) (*daemonproto.LinkResult, error) {
	f.mu.Lock()
	f.ensured = append(f.ensured, env)
	hook := f.OnEnsure
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return f.result(env)
}

func (f *fakeDaemon) RestartLink(_ context.Context, env string) (*daemonproto.LinkResult, error) {
	f.mu.Lock()
	f.restarts = append(f.restarts, env)
	f.mu.Unlock()
	return f.result(env)
}

func (f *fakeDaemon) Status(context.Context) (*daemonproto.StatusResult, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	if len(f.Stats) > 0 {
		st := f.Stats[0]
		if len(f.Stats) > 1 {
			f.Stats = f.Stats[1:]
		}
		return st, nil
	}
	if f.Stat == nil {
		return nil, errors.New("no status")
	}
	return f.Stat, nil
}

// fakeCode is the one-time code the fake daemon hands out.
var fakeCode = strings.Repeat("c", daemonproto.CodeLen)

// Web answers web: WebRes, or a listening page at 127.0.0.1:7171, with a
// code when one is asked for.
func (f *fakeDaemon) Web(_ context.Context, p daemonproto.WebParams) (*daemonproto.WebResult, error) {
	f.mu.Lock()
	f.webCalls = append(f.webCalls, p)
	f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	if f.WebRes != nil {
		return f.WebRes, nil
	}
	r := &daemonproto.WebResult{WebState: daemonproto.WebState{Listening: true, Addr: "127.0.0.1:7171"}, URL: "http://127.0.0.1:7171/"}
	if p.Code {
		r.CodeURL = r.URL + "#code=" + fakeCode
	}
	return r, nil
}

func (f *fakeDaemon) Close() error { return nil }

// newCLIApp is an App for the CLI paths: a data dir of its own, no sandbox
// (a Docker that is not there), and a daemon that is fake. Anything that
// would start a process fails the test.
func newCLIApp(t *testing.T, fd *fakeDaemon) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errb bytes.Buffer
	a := &App{
		Cfg: &config.Config{Env: "default", Container: "box", DataDir: t.TempDir(), Home: "/h", CabooseHome: "/h/.caboose",
			ForwardPorts: "3000", OpenURLs: "ask", Getenv: func(string) string { return "" }},
		Stdout: &out, Stderr: &errb, DaemonDial: fd.dial,
		Executable: func() (string, error) { return "/bin/caboose", nil },
		Spawn:      func(exe string, args ...string) error { t.Errorf("spawned %s %v", exe, args); return nil },
	}
	return a, &out, &errb
}

// A launch has the daemon run the link, starting the daemon if need be, and
// spawns no helper of its own.
func TestStartLinkAsksTheDaemon(t *testing.T) {
	fd := &fakeDaemon{}
	a, _, errb := newCLIApp(t, fd)
	a.startLink()
	if !slices.Equal(fd.ensured, []string{"default"}) || !slices.Equal(fd.dials, []bool{true}) {
		t.Fatalf("ensure_link %v, dials (start) %v", fd.ensured, fd.dials)
	}
	if errb.Len() != 0 {
		t.Errorf("said %q", errb)
	}
}

// When the daemon cannot be reached or refuses, the launch says so once,
// with the command to run, and neither fails nor falls back to a helper
// (newCLIApp's Spawn fails the test).
func TestStartLinkFailureIsOneNote(t *testing.T) {
	for name, fd := range map[string]*fakeDaemon{
		"no daemon":   {Dial: errors.New("the daemon did not answer within 10s of being started")},
		"refused":     {Err: daemonproto.NewError(daemonproto.KindInternal, "docker is not there")},
		"link failed": {Result: &daemonproto.LinkResult{Action: daemonproto.ActionStarted, Env: daemonproto.EnvStatus{Name: "default", Link: daemonproto.LinkFailed, LastError: "cannot lock link.lock"}}},
	} {
		t.Run(name, func(t *testing.T) {
			a, _, errb := newCLIApp(t, fd)
			a.startLink()
			s := errb.String()
			if strings.Count(s, "caboose: ") != 1 || !strings.Contains(s, "caboose daemon restart") || !strings.Contains(s, "no link to the sandbox") {
				t.Errorf("said %q", s)
			}
		})
	}
}

// A command run with CABOOSE_DATA_DIR set is not the daemon's: it keeps a
// detached helper, and never asks the daemon.
func TestStartLinkWithDataDirKeepsTheHelper(t *testing.T) {
	fd := &fakeDaemon{}
	a, _, _ := newCLIApp(t, fd)
	a.Cfg.Getenv = dataDirEnv
	spawned := spawnRecorder(a)
	a.startLink()
	if len(fd.dials) != 0 || len(*spawned) != 1 || strings.Join((*spawned)[0], " ") != "/bin/caboose --env default link --background" {
		t.Fatalf("dials %v, spawned %v", fd.dials, *spawned)
	}
}

// caboose link asks for the link; --restart for a new one; neither runs it
// here.
func TestLinkCommandAsksTheDaemon(t *testing.T) {
	fd := &fakeDaemon{}
	a, _, errb := newCLIApp(t, fd)
	if err := a.Link(nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fd.ensured, []string{"default"}) || len(fd.restarts) != 0 || !strings.Contains(errb.String(), "started the link") {
		t.Fatalf("ensure %v, restart %v, said %q", fd.ensured, fd.restarts, errb)
	}
	fd.Result = &daemonproto.LinkResult{Action: daemonproto.ActionReplaced, Env: daemonproto.EnvStatus{Name: "default", Link: daemonproto.LinkRunning}}
	if err := a.Link([]string{"--restart"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fd.restarts, []string{"default"}) || len(fd.ensured) != 1 || !strings.Contains(errb.String(), "replaced the link") {
		t.Fatalf("restart %v, ensure %v, said %q", fd.restarts, fd.ensured, errb)
	}
	// The sandbox does not run: said, not an error.
	fd.Result = &daemonproto.LinkResult{Action: daemonproto.ActionNoSandbox, Env: daemonproto.EnvStatus{Name: "default"}}
	errb.Reset()
	if err := a.Link(nil); err != nil || !strings.Contains(errb.String(), "is not running") {
		t.Fatalf("%v, said %q", err, errb)
	}
}

// An explicit link command fails, with the fix, when the daemon cannot run
// the link.
func TestLinkCommandFailsWithTheFix(t *testing.T) {
	for _, fd := range []*fakeDaemon{{Dial: errors.New("nope")}, {Err: daemonproto.NewError(daemonproto.KindInternal, "boom")}} {
		a, _, _ := newCLIApp(t, fd)
		err := a.Link(nil)
		if err == nil || !strings.Contains(err.Error(), "caboose daemon restart") {
			t.Errorf("got %v", err)
		}
	}
	a, _, _ := newCLIApp(t, &fakeDaemon{})
	for _, args := range [][]string{{"--foreground", "--restart"}, {"--background", "--foreground"}, {"--now"}} {
		if err := a.Link(args); err == nil || !strings.Contains(err.Error(), "usage: caboose link [--restart | --foreground | --background]") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// --foreground is refused while the daemon serves the environment's link,
// whether the daemon says so or link.json and the lock do.
func TestLinkForegroundRefusedWhileTheDaemonServes(t *testing.T) {
	fd := &fakeDaemon{Stat: &daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{
		{Name: "default", Sandbox: daemonproto.SandboxRunning, Link: daemonproto.LinkRunning}}}}
	a, _, _ := newCLIApp(t, fd)
	if err := a.Link([]string{"--foreground"}); err == nil || !strings.Contains(err.Error(), "daemon serves this environment's link") {
		t.Errorf("by the daemon's word: %v", err)
	}

	a, _, _ = newCLIApp(t, &fakeDaemon{})
	unlock, err := lockLink(a.Cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := writeLinkState(a.Cfg.DataDir, linkState{Daemon: "/h/.caboose/daemon", linkSettings: settingsOf(a.Cfg)}); err != nil {
		t.Fatal(err)
	}
	if err := a.Link([]string{"--foreground"}); err == nil || !strings.Contains(err.Error(), "/h/.caboose/daemon") {
		t.Errorf("by link.json and the lock: %v", err)
	}
}

// Status says how the daemon and the link are, whether or not the sandbox
// runs, and starts and creates nothing.
func TestStatusDaemonAndLinkLines(t *testing.T) {
	started := time.Date(2026, 10, 17, 9, 30, 0, 0, time.Local)
	daemonUp := func(envs ...daemonproto.EnvStatus) *daemonproto.StatusResult {
		return &daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{PID: 99, Version: "v9", Protocol: 1, Started: started}, Envs: envs}
	}
	running := daemonproto.EnvStatus{Name: "default", Sandbox: daemonproto.SandboxRunning, Link: daemonproto.LinkRunning, Since: started}
	for _, tc := range []struct {
		name      string
		stat      *daemonproto.StatusResult
		holdLock  bool
		wantDaemn string
		wantLink  string
	}{
		{"running", daemonUp(running), true, "running (pid 99, caboose v9) since 2026-10-17 09:30:00", "running since 2026-10-17 09:30:00"},
		// The link died, or never came: never reported as up.
		{"link stopped", daemonUp(daemonproto.EnvStatus{Name: "default", Link: daemonproto.LinkStopped}), false, "running (pid 99", "not running (the daemon's link is stopped); run: caboose link"},
		{"link failed", daemonUp(daemonproto.EnvStatus{Name: "default", Link: daemonproto.LinkFailed, LastError: "no docker"}), false, "running (pid 99", "not running (the daemon's link is failed: no docker); run: caboose link"},
		{"unknown env", daemonUp(), false, "running (pid 99", "not running (the daemon does not serve this environment); run: caboose link"},
		{"daemon says up, lock free", daemonUp(running), false, "running (pid 99", "not running (the daemon reports a link, but link.lock is free); run: caboose link"},
		{"no daemon", nil, false, "not running (", "not running (the daemon is not running); run: caboose daemon restart"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBoxApp(t, isolationContainer, runningBox(isolationContainer))
			b.daemon.Stat = tc.stat
			if err := os.MkdirAll(b.data, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.holdLock {
				unlock, err := lockLink(b.data)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			if err := b.Status(); err != nil {
				t.Fatalf("%v\n%s", err, b.errb)
			}
			out, _ := b.said()
			lines := map[string]string{}
			for _, l := range strings.Split(out, "\n") {
				if k, v, ok := strings.Cut(l, " : "); ok {
					lines[strings.TrimSpace(k)] = v
				}
			}
			if !strings.Contains(lines["daemon"], tc.wantDaemn) || lines["link"] != tc.wantLink {
				t.Errorf("daemon %q (want %q), link %q (want %q)\n%s", lines["daemon"], tc.wantDaemn, lines["link"], tc.wantLink, out)
			}
			for _, start := range b.daemon.dials {
				if start {
					t.Error("status started the daemon")
				}
			}
		})
	}
}

// The real client, with nothing at CABOOSE_HOME: status says the daemon is
// not running, starts nothing (Spawn fails the test) and creates nothing.
func TestStatusCreatesNothingAndStartsNoDaemon(t *testing.T) {
	tmp, err := os.MkdirTemp("", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := saferm.RemoveRoot(tmp); err != nil {
			t.Error(err)
		}
	})
	home := filepath.Join(tmp, "ch")
	for _, iso := range isolations {
		b := newBoxApp(t, iso, &backendtest.Fake{})
		b.App.DaemonDial = nil
		b.Cfg.CabooseHome = home
		b.Spawn = func(string, ...string) error { t.Error("started a process"); return nil }
		if err := b.Status(); err != nil {
			t.Fatal(err)
		}
		out, _ := b.said()
		if !strings.Contains(out, "daemon    : not running (") || !strings.Contains(out, "link      : not running (the daemon is not running)") {
			t.Errorf("under %s:\n%s", iso, out)
		}
	}
	if _, err := os.Lstat(home); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("status created %s (%v)", home, err)
	}
}

// With CABOOSE_DATA_DIR set the link is a helper's: status reads link.lock.
func TestStatusLinkWithDataDirIsTheHelpers(t *testing.T) {
	a, _, _ := newCLIApp(t, &fakeDaemon{})
	a.Cfg.Getenv = dataDirEnv
	if got := a.linkLine(a.linkView(), true); !strings.HasPrefix(got, "not running (CABOOSE_DATA_DIR is set") {
		t.Errorf("no helper: %q", got)
	}
	unlock, err := lockLink(a.Cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if got := a.linkLine(a.linkView(), true); !strings.HasPrefix(got, "running (a detached helper") {
		t.Errorf("a helper: %q", got)
	}
}

// doctorRows runs doctor's daemon checks and returns the rows by label.
func doctorRows(a *App, running bool) []finding {
	c := &checkup{}
	a.doctorDaemon(c, running)
	return c.rows
}

func rowOf(rows []finding, label string, l level) *finding {
	for i := range rows {
		if rows[i].label == label && rows[i].level == l {
			return &rows[i]
		}
	}
	return nil
}

func TestDoctorDaemonAndLink(t *testing.T) {
	now := time.Now()
	up := func(version string, envs ...daemonproto.EnvStatus) *fakeDaemon {
		return &fakeDaemon{Stat: &daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{PID: 7, Version: version, Started: now}, Envs: envs}}
	}
	link := daemonproto.EnvStatus{Name: "default", Sandbox: daemonproto.SandboxRunning, Link: daemonproto.LinkRunning}

	t.Run("not running, sandbox running", func(t *testing.T) {
		fd := &fakeDaemon{}
		a, _, _ := newCLIApp(t, fd)
		rows := doctorRows(a, true)
		if p := rowOf(rows, "daemon", levelProblem); p == nil || !strings.Contains(p.fix, "caboose daemon restart") {
			t.Errorf("daemon: %+v", rows)
		}
		if rowOf(rows, "link", levelUnchecked) == nil {
			t.Errorf("link: %+v", rows)
		}
		for _, start := range fd.dials {
			if start {
				t.Error("doctor started the daemon")
			}
		}
	})
	t.Run("not running, sandbox not", func(t *testing.T) {
		a, _, _ := newCLIApp(t, &fakeDaemon{})
		rows := doctorRows(a, false)
		if rowOf(rows, "daemon", levelNote) == nil || rowOf(rows, "link", levelOK) == nil {
			t.Errorf("rows %+v", rows)
		}
	})
	t.Run("does not answer", func(t *testing.T) {
		a, _, _ := newCLIApp(t, &fakeDaemon{Dial: daemonproto.NewError(daemonproto.KindTimeout, "no hello in 2s")})
		if p := rowOf(doctorRows(a, true), "daemon", levelProblem); p == nil || p.fix != "caboose daemon restart" {
			t.Errorf("got %+v", p)
		}
	})
	t.Run("healthy", func(t *testing.T) {
		a, _, _ := newCLIApp(t, up(currentVersion(), link))
		unlock, err := lockLink(a.Cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		rows := doctorRows(a, true)
		if rowOf(rows, "daemon", levelOK) == nil || rowOf(rows, "link", levelOK) == nil {
			t.Errorf("rows %+v", rows)
		}
		for _, r := range rows {
			if r.level == levelProblem {
				t.Errorf("problem: %+v", r)
			}
		}
	})
	t.Run("older daemon", func(t *testing.T) {
		a, _, _ := newCLIApp(t, up("v0.0.1", link))
		unlock, _ := lockLink(a.Cfg.DataDir)
		defer unlock()
		if p := rowOf(doctorRows(a, true), "daemon", levelProblem); p == nil || p.fix != "caboose daemon restart" || !strings.Contains(p.text, "v0.0.1") {
			t.Errorf("got %+v", p)
		}
	})
	t.Run("sandbox runs, link does not", func(t *testing.T) {
		a, _, _ := newCLIApp(t, up(currentVersion(), daemonproto.EnvStatus{Name: "default", Link: daemonproto.LinkFailed, LastError: "boom"}))
		p := rowOf(doctorRows(a, true), "link", levelProblem)
		if p == nil || !strings.HasPrefix(p.fix, "caboose link") || !strings.Contains(p.text, "boom") {
			t.Errorf("got %+v", p)
		}
	})
	t.Run("socket path too long", func(t *testing.T) {
		a, _, _ := newCLIApp(t, up(currentVersion(), link))
		a.Cfg.CabooseHome = "/" + strings.Repeat("long", 30)
		p := rowOf(doctorRows(a, true), "daemon", levelProblem)
		if p == nil || !strings.Contains(p.fix, "CABOOSE_HOME") {
			t.Errorf("got %+v", p)
		}
	})
	t.Run("data dir set, no helper", func(t *testing.T) {
		a, _, _ := newCLIApp(t, &fakeDaemon{})
		a.Cfg.Getenv = dataDirEnv
		rows := doctorRows(a, true)
		if p := rowOf(rows, "link", levelProblem); p == nil || p.fix != "caboose link --restart" {
			t.Errorf("rows %+v", rows)
		}
		if rowOf(rows, "daemon", levelProblem) != nil {
			t.Errorf("the daemon is not this command's, and is no problem: %+v", rows)
		}
	})
}

// Under vm the agent's fix no longer sends the user to a terminal whose
// SSH_AUTH_SOCK reaches the agent: the daemon takes it from a login shell
// or ssh_agent.
func TestAgentFixUnderVM(t *testing.T) {
	b := newBoxApp(t, isolationVM, runningBox(isolationVM))
	b.daemon.Stat = &daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{{Name: "default", Link: daemonproto.LinkRunning}}}
	unlock, err := lockLink(b.data)
	if err != nil {
		// the data dir does not exist yet
		if mkerr := os.MkdirAll(b.data, 0o700); mkerr != nil {
			t.Fatal(mkerr)
		}
		unlock, err = lockLink(b.data)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer unlock()
	fix := b.agentFix("no keys")
	for _, want := range []string{"ssh_agent", "IdentityAgent in ~/.ssh/config", "login shell", "caboose daemon restart"} {
		if !strings.Contains(fix, want) {
			t.Errorf("fix %q lacks %q", fix, want)
		}
	}
	if strings.Contains(fix, "terminal whose SSH_AUTH_SOCK") {
		t.Errorf("fix %q", fix)
	}
	// With no link running, that is the fix.
	b.daemon.Stat = &daemonproto.StatusResult{}
	if fix := b.agentFix("no keys"); !strings.HasPrefix(fix, "caboose link:") {
		t.Errorf("no link: %q", fix)
	}
}

// The commands against a real daemon (in this process, over a socket in a
// short temp dir) running real links: a launch's link comes from the
// daemon, a restart replaces it, status sees it, and a dead link is not
// reported as up.
func TestCommandsAgainstADaemon(t *testing.T) {
	short, err := os.MkdirTemp("", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := saferm.RemoveRoot(short); err != nil {
			t.Error(err)
		}
	})
	w := newLinksWorld(t)
	b := w.env("alpha", "running")
	var out, errb bytes.Buffer
	a := &App{
		Cfg: &config.Config{Env: "alpha", Container: "alpha-box", DataDir: w.dataDir("alpha"), Home: short, CabooseHome: short,
			ForwardPorts: "3000-3999", OpenURLs: "ask", Getenv: func(string) string { return "" }},
		Stdout: &out, Stderr: &errb, Backend: b,
		Executable: func() (string, error) { return "/bin/caboose", nil },
		Spawn:      func(exe string, args ...string) error { t.Errorf("spawned %s %v", exe, args); return nil },
	}
	dir := a.daemonDir()
	dev := a.installed().kind == installDev
	links := NewDaemonLinks(DaemonLinksOptions{Dir: dir, Dev: dev, CabooseHome: w.home, NewApp: w.newApp})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Options{Dir: dir, Version: currentVersion(), Dev: dev, Links: links})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("daemon: %v", err)
		}
		links.Close()
	})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if c, err := daemon.Connect(t.Context(), a.daemonConfig()); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon never answered")
		}
	}

	if v := a.linkView(); v.Up || v.Why == "" {
		t.Fatalf("before any link: %+v", v)
	}
	a.startLink()
	if errb.Len() != 0 {
		t.Fatalf("launch said %q", errb.String())
	}
	b.session(t)
	v := a.linkView()
	if !v.Up || v.Env == nil || v.Env.Link != daemonproto.LinkRunning {
		t.Fatalf("after the launch: %+v", v)
	}
	if got := a.linkLine(v, true); !strings.HasPrefix(got, "running since ") {
		t.Errorf("link line %q", got)
	}
	if err := a.Link([]string{"--foreground"}); err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Errorf("--foreground while the daemon serves: %v", err)
	}
	errb.Reset()
	if err := a.Link([]string{"--restart"}); err != nil || !strings.Contains(errb.String(), "replaced the link") {
		t.Fatalf("restart: %v, said %q", err, errb.String())
	}
	b.session(t)

	// The link goes away under the daemon (its lock is released): not up.
	if _, err := links.Serve(t.Context(), daemonproto.MethodStopLink, "alpha"); err != nil {
		t.Fatal(err)
	}
	if v := a.linkView(); v.Up {
		t.Errorf("a stopped link reads as up: %+v", v)
	}
}

// A daemon that has just started and is still bringing back its links is
// waited for, briefly: an environment it lists by then is read as it says,
// and one it does not yet is said to be on its way, never "not served".
func TestLinkViewWaitsForRecovery(t *testing.T) {
	recovering := &daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{PID: 7, Recovering: true}, Envs: []daemonproto.EnvStatus{}}
	back := &daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{PID: 7},
		Envs: []daemonproto.EnvStatus{{Name: "default", Sandbox: daemonproto.SandboxRunning, Link: daemonproto.LinkRunning}}}

	fd := &fakeDaemon{Stat: recovering, Stats: []*daemonproto.StatusResult{recovering, recovering, back}}
	a, _, _ := newCLIApp(t, fd)
	v := a.linkView()
	if v.Env == nil || v.Env.Link != daemonproto.LinkRunning || fd.statusCalls != 3 {
		t.Fatalf("after recovery: %+v, %d status calls", v, fd.statusCalls)
	}

	old := recoveryWait
	recoveryWait = 100 * time.Millisecond
	t.Cleanup(func() { recoveryWait = old })
	fd = &fakeDaemon{Stat: recovering, Stats: []*daemonproto.StatusResult{recovering}}
	a, _, _ = newCLIApp(t, fd)
	v = a.linkView()
	if v.Up || v.Env != nil || !strings.Contains(v.Why, "still bringing back the links it ran") {
		t.Fatalf("still recovering: %+v", v)
	}

	// A daemon done recovering is not waited for.
	fd = &fakeDaemon{Stat: back, Stats: []*daemonproto.StatusResult{{Daemon: daemonproto.DaemonInfo{PID: 7}, Envs: []daemonproto.EnvStatus{}}}}
	a, _, _ = newCLIApp(t, fd)
	if v := a.linkView(); v.Why != "the daemon does not serve this environment" || fd.statusCalls != 1 {
		t.Fatalf("recovered, not serving: %+v, %d status calls", v, fd.statusCalls)
	}
}
