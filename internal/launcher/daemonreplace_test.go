package launcher

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/saferm"
	"github.com/bfreis/caboose/internal/selfupdate"
)

// shortHome is a CABOOSE_HOME short enough for the daemon's socket,
// removed at the test's end.
func shortHome(t *testing.T) string {
	t.Helper()
	short, err := os.MkdirTemp("", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := saferm.RemoveRoot(short); err != nil {
			t.Error(err)
		}
	})
	return short
}

// inProcessDaemon runs a daemon in this process, until the test's end,
// and returns once it serves its socket (its Ready: the socket file exists
// as soon as it is bound, before it listens); done is closed once it has
// stopped. opts.Ready must be unset.
func inProcessDaemon(t *testing.T, opts daemon.Options) (done chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan struct{})
	ready := make(chan struct{})
	opts.Ready = func(*log.Logger) { close(ready) }
	go func() {
		defer close(done)
		if err := daemon.Run(ctx, opts); err != nil {
			t.Errorf("daemon: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-ready:
		return done
	case <-done:
		t.Fatal("the daemon stopped before it served")
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon never served")
	}
	return nil
}

// slowLinks are no links that take a while to stop, as a daemon's links
// do when they have commands to kill: the daemon lets go of its lock only
// after, which widens the window a client that spawns too early would
// hit.
type slowLinks struct{ stop time.Duration }

func (slowLinks) Serve(context.Context, string, string) (*daemonproto.LinkResult, error) {
	return nil, daemonproto.NewError(daemonproto.KindUnknownMethod, "no links")
}
func (slowLinks) Envs() []daemonproto.EnvStatus { return []daemonproto.EnvStatus{} }
func (slowLinks) Busy() bool                    { return false }
func (slowLinks) HostExecs() int                { return 0 }
func (slowLinks) DrainHostExecs(bool) bool      { return true }
func (slowLinks) Recovering() bool              { return false }
func (l slowLinks) Close()                      { time.Sleep(l.stop) }

// lockHeld reports whether something holds the flock on path, which
// exists.
func lockHeld(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true
		}
		t.Fatal(err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

func replaceApp(t *testing.T) *App {
	home := shortHome(t)
	return &App{
		Cfg:    &config.Config{Env: "default", Home: home, CabooseHome: home, Getenv: func(string) string { return "" }},
		Stdout: io.Discard, Stderr: io.Discard,
		Executable: func() (string, error) { return "/bin/caboose", nil },
	}
}

// A caboose of a newer protocol replaces the running daemon when it may
// start one, and connects to its own; one that only asks leaves it and
// says it is older.
func TestDialDaemonReplacesAnOlderProtocol(t *testing.T) {
	a := replaceApp(t)
	old := daemonProtocol
	daemonProtocol = daemonproto.ProtocolVersion + 1
	t.Cleanup(func() { daemonProtocol = old })
	dir := a.daemonDir()
	first := inProcessDaemon(t, daemon.Options{Dir: dir, Version: "v1.0.0", Idle: time.Minute, Log: io.Discard,
		Links: slowLinks{stop: 300 * time.Millisecond}})

	// Asking only: refused as older, nothing replaced.
	if _, err := a.dialDaemon(context.Background(), false); daemonproto.Kind(err) != daemonproto.KindProtocolMismatch ||
		!strings.Contains(err.Error(), "older caboose's daemon") || !strings.Contains(err.Error(), "caboose daemon restart") {
		t.Fatalf("dial without start: %v", err)
	}
	if v := a.linkView(); !strings.Contains(a.daemonLine(v), "the next launch replaces it") {
		t.Errorf("daemon line %q", a.daemonLine(v))
	}
	select {
	case <-first:
		t.Fatal("asking replaced the daemon")
	case <-time.After(100 * time.Millisecond):
	}

	var spawned []string
	a.Spawn = func(exe string, args ...string) error {
		spawned = append(spawned, exe+" "+strings.Join(args, " "))
		// The old daemon is gone once it has let go of its lock, having
		// removed its socket and daemon.json first. Its goroutine
		// returns from Run only after that, so done may still be open.
		if lockHeld(t, filepath.Join(dir, daemon.LockFile)) {
			t.Error("spawned before the old daemon let go of its lock")
		}
		for _, f := range []string{daemon.SocketFile, daemon.StateFile} {
			if _, err := os.Lstat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("spawned with the old daemon's %s still there (%v)", f, err)
			}
		}
		select {
		case <-first:
		case <-time.After(5 * time.Second):
			t.Error("the old daemon's Run never returned")
		}
		inProcessDaemon(t, daemon.Options{Dir: dir, Version: "v2.0.0", Idle: time.Minute, Log: io.Discard, Protocol: daemonProtocol})
		return nil
	}
	c, err := a.dialDaemon(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(spawned) != 1 || spawned[0] != "/bin/caboose daemon" {
		t.Errorf("spawned %v", spawned)
	}
	if cl := c.(*daemonproto.Client); cl.Daemon().Version != "v2.0.0" {
		t.Errorf("connected to %+v", cl.Daemon())
	}
}

// fakeReleaseDaemon is the release's daemon as an update asks it.
type fakeReleaseDaemon struct {
	versions []string
	replaced []string
	result   *daemonproto.ReplaceResult
}

func (f *fakeReleaseDaemon) Versions(context.Context) []string { return f.versions }
func (f *fakeReleaseDaemon) Replace(_ context.Context, tag string) (*daemonproto.ReplaceResult, error) {
	f.replaced = append(f.replaced, tag)
	if f.result == nil {
		return nil, daemonproto.NewError(daemonproto.KindUnavailable, "none")
	}
	return f.result, nil
}

// After an install, the release's daemon is asked to make way for the new
// version, and its own version's binary is kept; after a failed one it is
// not asked.
func TestUpdateReplacesTheDaemon(t *testing.T) {
	for _, background := range []bool{false, true} {
		e := newUpdateEnv(t)
		e.srv.Publish(t, "v1.2.0", true, goosArch())
		// v1.0.0 runs the daemon; the update runs from v1.1.0, installed
		// beside it, so v1.0.0 is neither the new one nor the one before.
		l := e.a.layout()
		goos, goarch := splitPlat()
		if err := l.Install(context.Background(), selfupdate.Source{Base: e.srv.Base}, "v1.1.0", goos, goarch, ""); err != nil {
			t.Fatal(err)
		}
		e.a.Executable = func() (string, error) { return filepath.Join(l.Versions, "v1.1.0", "caboose"), nil }
		e.version("v1.1.0")
		fd := &fakeReleaseDaemon{versions: []string{"v1.0.0"}, result: &daemonproto.ReplaceResult{HostExecs: 1}}
		e.a.relDaemon = fd
		var err error
		if background {
			err = e.a.Update([]string{"--background"})
		} else {
			err = e.a.Update(nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		if e.current() != "v1.2.0" || len(fd.replaced) != 1 || fd.replaced[0] != "v1.2.0" {
			t.Fatalf("background %v: current %s, replaced %v", background, e.current(), fd.replaced)
		}
		if _, err := os.Stat(filepath.Join(l.Versions, "v1.0.0", "caboose")); err != nil {
			t.Errorf("background %v: the daemon's version was pruned: %v", background, err)
		}
		if !background && !strings.Contains(e.out.String(), "restarts as v1.2.0 once no host command runs") {
			t.Errorf("said:\n%s", e.out)
		}
	}

	// A failed install asks nothing.
	e := newUpdateEnv(t)
	fd := &fakeReleaseDaemon{result: &daemonproto.ReplaceResult{Exiting: true}}
	e.a.relDaemon = fd
	goos, goarch := splitPlat()
	e.srv.Replace("v1.1.0", selfupdate.AssetName("v1.1.0", goos, goarch), []byte("broken"))
	if err := e.a.Update(nil); err == nil {
		t.Fatal("a broken download installed")
	}
	_ = e.a.Update([]string{"--background"})
	if len(fd.replaced) != 0 {
		t.Errorf("replaced after a failed install: %v", fd.replaced)
	}
}

func splitPlat() (string, string) {
	goos, goarch, _ := strings.Cut(goosArch(), "/")
	return goos, goarch
}

// respawnWorld is an install of v1.0.0 and v1.1.0 under a short home, for
// the daemon that exits for a replace.
func respawnWorld(t *testing.T) (*App, *[]string) {
	a := replaceApp(t)
	l := a.layout()
	for _, v := range []string{"v1.0.0", "v1.1.0"} {
		if err := os.MkdirAll(filepath.Join(l.Versions, v), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(l.Versions, v, "caboose"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var spawned []string
	a.Spawn = func(exe string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		spawned = append(spawned, exe+" "+strings.Join(args, " "))
		return nil
	}
	old := ensureDaemon
	ensureDaemon = func(_ context.Context, cfg daemon.Config) (*daemonproto.Client, error) {
		if cfg.Dir != a.daemonDir() {
			t.Errorf("ensured %s", cfg.Dir)
		}
		if err := cfg.Spawn(); err != nil {
			return nil, err
		}
		return nil, errors.New("not waited for in a test")
	}
	t.Cleanup(func() { ensureDaemon = old })
	return a, &spawned
}

// The daemon that exits for a replace starts the version it names, only
// after a replace that was not forced, with a link running, and when that
// version is installed.
func TestRespawnAfterReplace(t *testing.T) {
	var logged bytes.Buffer
	lg := log.New(&logged, "", 0)
	for name, c := range map[string]struct {
		st   daemon.Stopped
		want bool
	}{
		"replaced, busy":     {daemon.Stopped{Replaced: true, Version: "v1.1.0", Busy: true}, true},
		"replaced, idle":     {daemon.Stopped{Replaced: true, Version: "v1.1.0"}, false},
		"forced":             {daemon.Stopped{Forced: true, Version: "v1.1.0", Busy: true}, false},
		"stopped":            {daemon.Stopped{Why: "asked to stop", Busy: true}, false},
		"not installed":      {daemon.Stopped{Replaced: true, Version: "v1.2.0", Busy: true}, false},
		"not a release":      {daemon.Stopped{Replaced: true, Version: "dev", Busy: true}, false},
		"escaping a version": {daemon.Stopped{Replaced: true, Version: "../v1.1.0", Busy: true}, false},
	} {
		t.Run(name, func(t *testing.T) {
			a, spawned := respawnWorld(t)
			a.respawnDaemon(context.Background(), c.st, lg)
			want := filepath.Join(a.layout().Versions, "v1.1.0", "caboose") + " daemon"
			if got := len(*spawned) == 1 && (*spawned)[0] == want; got != c.want || (!c.want && len(*spawned) != 0) {
				t.Errorf("spawned %v, want %v", *spawned, c.want)
			}
		})
	}
}
