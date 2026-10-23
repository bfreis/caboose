package daemon

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// fakeLinks counts host-exec requests in flight, as DaemonLinks' gate
// does, and records what the daemon does to it.
type fakeLinks struct {
	mu       sync.Mutex
	inFlight int
	drained  bool
	held     bool // a drain holds new requests off
	busy     bool
	closed   int
	killed   int // requests in flight when Close ran
	// recovering is what Recovering says.
	recovering bool
	// closeTakes is how long Close takes, as ending host commands does.
	closeTakes time.Duration
	// jobs is how many stop or restart jobs ExecCounts says run, which
	// HostExecs counts beside the commands.
	jobs int
}

// ExecCounts is the commands in flight and the jobs, in one read.
func (f *fakeLinks) ExecCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight, f.jobs
}

func (f *fakeLinks) Serve(context.Context, string, string) (*daemonproto.LinkResult, error) {
	return nil, daemonproto.NewError(daemonproto.KindUnknownMethod, "none")
}
func (f *fakeLinks) Envs() []daemonproto.EnvStatus { return nil }
func (f *fakeLinks) Busy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy
}
func (f *fakeLinks) HostExecs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight + f.jobs
}

func (f *fakeLinks) Recovering() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recovering
}

// DrainHostExecs is the gate's Drain.
func (f *fakeLinks) DrainHostExecs(hold bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inFlight == 0 {
		f.drained = true
		return true
	}
	f.held = hold
	return false
}
func (f *fakeLinks) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	f.killed += f.inFlight
	time.Sleep(f.closeTakes)
}

// enter is a host-exec request admitted, as the gate admits one.
func (f *fakeLinks) enter() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.drained || f.held {
		return false
	}
	f.inFlight++
	return true
}

func (f *fakeLinks) leave() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
}

func (f *fakeLinks) state() (closed, killed int, drained bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed, f.killed, f.drained
}

// replaceDaemon is a daemon in this process speaking protocol 2, so that
// clients of protocols 1, 2 and 3 are older, the same and newer.
type replaceDaemon struct {
	t       *testing.T
	dir     string
	links   *fakeLinks
	stopped chan Stopped
	// noLockCheck: another daemon may take the lock as soon as this one
	// lets go of it.
	noLockCheck bool
	logs        *syncBuf
}

// syncBuf is a log written by the daemon and read by the test.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const replaceProto = 2

func startReplaceDaemon(t *testing.T, version string, links *fakeLinks) *replaceDaemon {
	t.Helper()
	old := replaceRecheck
	replaceRecheck = 20 * time.Millisecond
	t.Cleanup(func() { replaceRecheck = old })
	h := newHarness(t)
	d := &replaceDaemon{t: t, dir: h.dir, links: links, stopped: make(chan Stopped, 1), logs: &syncBuf{}}
	runInProcess(t, Options{Dir: h.dir, Version: version, Idle: time.Minute, Log: io.MultiWriter(testLog{t}, d.logs), Links: links, Protocol: replaceProto,
		OnStop: func(st Stopped, lg *log.Logger) {
			if lg == nil {
				t.Error("OnStop got no log")
			}
			d.stopped <- st
		}})
	return d
}

func (d *replaceDaemon) client(protocol int) (*daemonproto.Client, error) {
	c, err := daemonproto.Dial(context.Background(), Socket(d.dir), daemonproto.Options{Client: daemonproto.ClientCLI, Version: "v9.9.9", Protocol: protocol})
	if err == nil {
		d.t.Cleanup(func() { c.Close() })
	}
	return c, err
}

func (d *replaceDaemon) mustClient(protocol int) *daemonproto.Client {
	d.t.Helper()
	c, err := d.client(protocol)
	if err != nil {
		d.t.Fatalf("dial (protocol %d): %v", protocol, err)
	}
	return c
}

// waitStopped is what made the daemon stop, once it has let go of its lock.
func (d *replaceDaemon) waitStopped(within time.Duration) Stopped {
	d.t.Helper()
	select {
	case st := <-d.stopped:
		if h, _ := held(filepath.Join(d.dir, LockFile)); h && !d.noLockCheck {
			d.t.Error("OnStop ran with the lock held")
		}
		return st
	case <-time.After(within):
		d.t.Fatalf("the daemon did not stop within %v", within)
	}
	return Stopped{}
}

func (d *replaceDaemon) running() bool {
	select {
	case st := <-d.stopped:
		d.stopped <- st
		return false
	default:
	}
	h, _ := held(filepath.Join(d.dir, LockFile))
	return h
}

// A replace that is not forced waits while a host command runs: the daemon
// answers and holds its lock, and exits once the command ends.
func TestReplaceWaitsForHostExecs(t *testing.T) {
	links := &fakeLinks{busy: true}
	links.enter()
	d := startReplaceDaemon(t, "v1.0.0", links)
	c := d.mustClient(replaceProto)
	r, err := c.Replace(context.Background(), "v1.1.0", false)
	if err != nil || r.Exiting || r.HostExecs != 1 || r.Current {
		t.Fatalf("replace: %+v, %v", r, err)
	}
	// A second one starts no other wait, and says the same.
	if r, err := d.mustClient(replaceProto).Replace(context.Background(), "v1.2.0", false); err != nil || r.Exiting || r.HostExecs != 1 {
		t.Fatalf("second replace: %+v, %v", r, err)
	}
	time.Sleep(200 * time.Millisecond)
	if !d.running() {
		t.Fatal("exited with a host command running")
	}
	st, err := d.mustClient(replaceProto).Status(context.Background())
	if err != nil || !st.Daemon.Replacing || st.Daemon.HostExecs != 1 || st.Daemon.Protocol != replaceProto {
		t.Fatalf("status while waiting: %+v, %v", st, err)
	}
	// While it waits, no new host command is admitted, so a steady stream
	// of them cannot hold the replace off.
	if links.enter() {
		t.Fatal("a host-exec request admitted while the replace waits")
	}
	links.leave()
	got := d.waitStopped(5 * time.Second)
	if !got.Replaced || got.Forced || got.Version != "v1.2.0" || !got.Busy {
		t.Errorf("stopped %+v", got)
	}
	if closed, killed, drained := links.state(); closed != 1 || killed != 0 || !drained {
		t.Errorf("closed %d, killed %d, drained %v", closed, killed, drained)
	}
	if links.enter() {
		t.Error("a host-exec request admitted after the drain")
	}
}

// The hold on new host commands while a replace waits is bounded: once it
// is over they are admitted again, and the replace still happens once
// they have all ended.
func TestReplaceHoldIsBounded(t *testing.T) {
	oldHold, oldCycle := replaceHold, replaceCycle
	replaceHold, replaceCycle = 150*time.Millisecond, time.Hour
	t.Cleanup(func() { replaceHold, replaceCycle = oldHold, oldCycle })
	links := &fakeLinks{busy: true}
	links.enter()
	d := startReplaceDaemon(t, "v1.0.0", links)
	if r, err := d.mustClient(replaceProto).Replace(context.Background(), "v1.1.0", false); err != nil || r.Exiting {
		t.Fatalf("replace: %+v, %v", r, err)
	}
	if links.enter() {
		t.Fatal("admitted during the hold")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !links.enter() {
		if time.Now().After(deadline) {
			t.Fatal("still refused long after the hold")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !d.running() {
		t.Fatal("exited with host commands running")
	}
	links.leave()
	links.leave()
	if got := d.waitStopped(5 * time.Second); !got.Replaced {
		t.Errorf("stopped %+v", got)
	}
}

func TestHolding(t *testing.T) {
	t0 := time.Unix(1000, 0)
	for _, tc := range []struct {
		after time.Duration
		want  bool
	}{{0, true}, {replaceHold - time.Second, true}, {replaceHold, false},
		{replaceCycle - time.Second, false}, {replaceCycle, true}, {replaceCycle + replaceHold, false}} {
		if got := holding(t0, t0.Add(tc.after)); got != tc.want {
			t.Errorf("after %v: holding %v, want %v", tc.after, got, tc.want)
		}
	}
}

// With no host command, a replace exits at once.
func TestReplaceWithNoneExits(t *testing.T) {
	links := &fakeLinks{}
	d := startReplaceDaemon(t, "v1.0.0", links)
	r, err := d.mustClient(replaceProto).Replace(context.Background(), "v1.1.0", false)
	if err != nil || !r.Exiting || r.HostExecs != 0 {
		t.Fatalf("replace: %+v, %v", r, err)
	}
	got := d.waitStopped(2 * time.Second)
	if !got.Replaced || got.Version != "v1.1.0" || got.Busy {
		t.Errorf("stopped %+v", got)
	}
}

// A forced replace from a client of a newer protocol exits at once, the
// host commands killed with the links.
func TestReplaceForcedByNewerProtocol(t *testing.T) {
	links := &fakeLinks{busy: true}
	links.enter()
	links.enter()
	d := startReplaceDaemon(t, "v1.0.0", links)
	r, err := d.mustClient(replaceProto+1).Replace(context.Background(), "v2.0.0", true)
	if err != nil || !r.Exiting || r.HostExecs != 2 {
		t.Fatalf("replace: %+v, %v", r, err)
	}
	got := d.waitStopped(2 * time.Second)
	if got.Replaced || !got.Forced {
		t.Errorf("stopped %+v", got)
	}
	if closed, killed, _ := links.state(); closed != 1 || killed != 2 {
		t.Errorf("closed %d, killed %d", closed, killed)
	}
}

// A forced replace while one waits still exits at once.
func TestReplaceForcedWhileWaiting(t *testing.T) {
	links := &fakeLinks{}
	links.enter()
	d := startReplaceDaemon(t, "v1.0.0", links)
	if r, err := d.mustClient(replaceProto).Replace(context.Background(), "v1.1.0", false); err != nil || r.Exiting {
		t.Fatalf("replace: %+v, %v", r, err)
	}
	if r, err := d.mustClient(replaceProto+1).Replace(context.Background(), "v2.0.0", true); err != nil || !r.Exiting {
		t.Fatalf("forced: %+v, %v", r, err)
	}
	if got := d.waitStopped(2 * time.Second); got.Replaced || !got.Forced {
		t.Errorf("stopped %+v", got)
	}
}

// A client of the daemon's own protocol may not force: refused, and
// nothing stops or is killed.
func TestReplaceForceRefusedFromSameProtocol(t *testing.T) {
	links := &fakeLinks{busy: true}
	links.enter()
	d := startReplaceDaemon(t, "v1.0.0", links)
	c := d.mustClient(replaceProto)
	_, err := c.Replace(context.Background(), "v2.0.0", true)
	if daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Fatalf("forced replace from the same protocol: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !d.running() {
		t.Fatal("the daemon stopped")
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("ping after: %v", err)
	}
	if closed, killed, drained := links.state(); closed != 0 || killed != 0 || drained {
		t.Errorf("closed %d, killed %d, drained %v", closed, killed, drained)
	}
}

// A replace naming the daemon's version, or an older one, does nothing.
func TestReplaceCurrentIsNoop(t *testing.T) {
	links := &fakeLinks{}
	d := startReplaceDaemon(t, "v1.2.0", links)
	c := d.mustClient(replaceProto)
	for _, v := range []string{"v1.2.0", "v1.1.9", "v1.2.0-rc.1"} {
		r, err := c.Replace(context.Background(), v, false)
		if err != nil || !r.Current || r.Exiting {
			t.Errorf("replace %s: %+v, %v", v, r, err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if !d.running() {
		t.Fatal("the daemon stopped")
	}
	if st, err := c.Status(context.Background()); err != nil || st.Daemon.Replacing {
		t.Errorf("status %+v, %v", st, err)
	}
	if _, _, drained := links.state(); drained {
		t.Error("drained host exec")
	}
}

// A client of an older protocol is refused at the hello, and so cannot
// replace the daemon.
func TestReplaceOlderProtocolRefusedAtHello(t *testing.T) {
	links := &fakeLinks{}
	d := startReplaceDaemon(t, "v1.0.0", links)
	if _, err := d.client(replaceProto - 1); daemonproto.Kind(err) != daemonproto.KindProtocolMismatch {
		t.Fatalf("older client: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !d.running() {
		t.Fatal("the daemon stopped")
	}
	if closed, _, drained := links.state(); closed != 0 || drained {
		t.Error("the links were touched")
	}
}

// Ensure, from a client of a newer protocol, replaces the running daemon
// at once and starts its own.
func TestEnsureReplacesAnOlderProtocol(t *testing.T) {
	links := &fakeLinks{busy: true}
	links.enter() // a host command runs: a forced replace does not wait for it
	d := startReplaceDaemon(t, "v1.0.0", links)
	d.noLockCheck = true
	spawned := 0
	cfg := Config{Dir: d.dir, Version: "v2.0.0", Protocol: replaceProto + 1, Spawn: func() error {
		spawned++
		if h, _ := held(filepath.Join(d.dir, LockFile)); h {
			t.Error("spawned while the old daemon held its lock")
		}
		runInProcess(t, Options{Dir: d.dir, Version: "v2.0.0", Idle: time.Minute, Log: testLog{t}, Protocol: replaceProto + 1})
		return nil
	}}
	c, err := Ensure(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := d.waitStopped(time.Second); !got.Forced {
		t.Errorf("the old daemon stopped: %+v", got)
	}
	if spawned != 1 || c.Daemon().Version != "v2.0.0" || c.Compat() != daemonproto.CompatSame {
		t.Errorf("spawned %d, connected to %+v (compat %v)", spawned, c.Daemon(), c.Compat())
	}
	if _, killed, _ := links.state(); killed != 1 {
		t.Errorf("killed %d", killed)
	}
	// Without Spawn (status, doctor) nothing is replaced.
	links2 := &fakeLinks{}
	d2 := startReplaceDaemon(t, "v1.0.0", links2)
	c2, err := Ensure(context.Background(), Config{Dir: d2.dir, Version: "v2.0.0", Protocol: replaceProto + 1})
	if err != nil || c2.Compat() != daemonproto.CompatClientNewer {
		t.Fatalf("no spawn: %v", err)
	}
	c2.Close()
	if !d2.running() {
		t.Error("replaced without Spawn")
	}
}

// RunningVersions creates nothing, and reads daemon.json when the daemon
// speaks another protocol.
func TestRunningVersions(t *testing.T) {
	h := newHarness(t)
	if vs := RunningVersions(context.Background(), Config{Dir: h.dir, Version: "v"}); len(vs) != 0 {
		t.Errorf("none running: %v", vs)
	}
	if exists(h.dir) {
		t.Error("created the daemon's directory")
	}
	d := startReplaceDaemon(t, "v1.4.0", &fakeLinks{})
	for _, proto := range []int{replaceProto, replaceProto - 1} {
		vs := RunningVersions(context.Background(), Config{Dir: d.dir, Version: "v", Protocol: proto})
		if len(vs) != 1 || vs[0] != "v1.4.0" {
			t.Errorf("protocol %d: %v", proto, vs)
		}
	}
}

// With NoReplace, Ensure never replaces a daemon of an older protocol: it
// returns it connected, spawns nothing, and the daemon keeps running.
func TestEnsureNoReplaceKeepsAnOlderProtocol(t *testing.T) {
	links := &fakeLinks{}
	d := startReplaceDaemon(t, "v1.0.0", links)
	spawned := 0
	cfg := Config{Dir: d.dir, Version: "v2.0.0", Client: daemonproto.ClientApp, Protocol: replaceProto + 1, NoReplace: true,
		Spawn: func() error { spawned++; return nil }}
	c, err := Ensure(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Compat() != daemonproto.CompatClientNewer || c.Daemon().Version != "v1.0.0" || spawned != 0 {
		t.Errorf("connected to %+v (compat %v), spawned %d", c.Daemon(), c.Compat(), spawned)
	}
	// A replace (forced or not, with no host command running) would have
	// had the daemon exit before Ensure returned.
	if !d.running() {
		t.Error("the older daemon was replaced")
	}
	select {
	case st := <-d.stopped:
		t.Errorf("the daemon stopped: %+v", st)
	default:
	}
}

// The same when the older daemon turns up while Ensure waits for
// start.lock: it is found under the lock and returned, never replaced.
func TestEnsureNoReplaceUnderStartLock(t *testing.T) {
	h := newHarness(t)
	if err := prepareDir(h.dir); err != nil {
		t.Fatal(err)
	}
	lk, err := lockWait(context.Background(), filepath.Join(h.dir, StartLockFile), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	spawned := 0
	cfg := Config{Dir: h.dir, Version: "v2.0.0", Client: daemonproto.ClientApp, Protocol: replaceProto + 1, NoReplace: true,
		Spawn: func() error { spawned++; return nil }}
	type res struct {
		c   *daemonproto.Client
		err error
	}
	got := make(chan res, 1)
	go func() {
		c, err := Ensure(context.Background(), cfg)
		got <- res{c, err}
	}()
	// Whether Ensure saw nothing before the lock or the daemon after it,
	// it must not replace it.
	time.Sleep(50 * time.Millisecond)
	stopped := make(chan Stopped, 1)
	runInProcess(t, Options{Dir: h.dir, Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}, Links: &fakeLinks{}, Protocol: replaceProto,
		OnStop: func(st Stopped, _ *log.Logger) { stopped <- st }})
	unlock(lk)
	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.c.Close()
	if r.c.Compat() != daemonproto.CompatClientNewer || spawned != 0 {
		t.Errorf("compat %v, spawned %d", r.c.Compat(), spawned)
	}
	select {
	case st := <-stopped:
		t.Errorf("the daemon stopped: %+v", st)
	default:
	}
	if c2, err := Connect(context.Background(), Config{Dir: h.dir, Version: "v1.0.0", Protocol: replaceProto}); err != nil {
		t.Errorf("the daemon is gone: %v", err)
	} else {
		c2.Close()
	}
}

// Status says whether the links are still being brought back, as they
// say it at each call.
func TestStatusSaysRecovering(t *testing.T) {
	links := &fakeLinks{recovering: true}
	d := startReplaceDaemon(t, "v1.0.0", links)
	c := d.mustClient(replaceProto)
	st, err := c.Status(t.Context())
	if err != nil || !st.Daemon.Recovering {
		t.Fatalf("while recovering: %+v, %v", st, err)
	}
	links.mu.Lock()
	links.recovering = false
	links.mu.Unlock()
	if st, err := c.Status(t.Context()); err != nil || st.Daemon.Recovering {
		t.Fatalf("once recovered: %+v, %v", st, err)
	}
}

// A stop waits longer for a daemon that is ending host commands, says
// so first, and names them if even that is not enough; with none it
// waits StopWait alone.
func TestStopWaitsForTheHostCommandsEnd(t *testing.T) {
	for _, c := range []struct {
		name     string
		inFlight int
		execWait time.Duration
		wantErr  string
	}{
		{"commands, ended in time", 1, 5 * time.Second, ""},
		{"commands, too slow", 2, 100 * time.Millisecond, "did not stop within 200ms, ending 2 host command(s)"},
		{"no commands", 0, 5 * time.Second, "did not stop within 100ms"},
	} {
		t.Run(c.name, func(t *testing.T) {
			links := &fakeLinks{inFlight: c.inFlight, closeTakes: 600 * time.Millisecond}
			d := startReplaceDaemon(t, "v1.0.0", links)
			var told []int
			cfg := Config{Dir: d.dir, Version: "v1.0.0", Protocol: replaceProto, StopWait: 100 * time.Millisecond, StopExecWait: c.execWait,
				Stopping: func(n int, wait time.Duration) {
					told = append(told, n)
					if wait != 100*time.Millisecond+c.execWait {
						t.Errorf("told it waits %v", wait)
					}
				}}
			running, err := Stop(context.Background(), cfg)
			if !running {
				t.Error("not running")
			}
			if c.wantErr == "" && err != nil || c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Errorf("Stop: %v, want %q", err, c.wantErr)
			}
			if c.inFlight > 0 && (len(told) != 1 || told[0] != c.inFlight) || c.inFlight == 0 && len(told) != 0 {
				t.Errorf("told %v", told)
			}
			// Gone in the end, either way.
			waitFor(t, 5*time.Second, "the lock is free", func() bool {
				h, _ := held(filepath.Join(d.dir, LockFile))
				return !h
			})
		})
	}
}

// The forced-replace log, the replace's answer and the status each say how
// many host commands and how many stop or restart jobs, never one count
// for both.
func TestReplaceAndStatusSeparateCommandsFromJobs(t *testing.T) {
	links := &fakeLinks{busy: true, jobs: 1}
	links.enter()
	links.enter()
	d := startReplaceDaemon(t, "v1.0.0", links)
	st, err := d.mustClient(replaceProto).Status(context.Background())
	if err != nil || st.Daemon.HostExecs != 3 || st.Daemon.Jobs != 1 {
		t.Fatalf("status %+v, %v", st.Daemon, err)
	}
	r, err := d.mustClient(replaceProto+1).Replace(context.Background(), "v2.0.0", true)
	if err != nil || !r.Exiting || r.HostExecs != 3 || r.Jobs != 1 {
		t.Fatalf("replace: %+v, %v", r, err)
	}
	d.waitStopped(2 * time.Second)
	if got, want := d.logs.String(), "killing 2 host command(s) and ending 1 stop/restart job(s)"; !strings.Contains(got, want) {
		t.Errorf("log %q lacks %q", got, want)
	}
}

// A replace that waits says the same of what it waits for.
func TestWaitingReplaceLogSeparatesCommandsFromJobs(t *testing.T) {
	links := &fakeLinks{jobs: 2}
	links.enter()
	d := startReplaceDaemon(t, "v1.0.0", links)
	r, err := d.mustClient(replaceProto).Replace(context.Background(), "v1.2.0", false)
	if err != nil || r.Exiting || r.HostExecs != 3 || r.Jobs != 2 {
		t.Fatalf("replace: %+v, %v", r, err)
	}
	if got, want := d.logs.String(), "host commands (1 running) and stop/restart jobs (2 running)"; !strings.Contains(got, want) {
		t.Errorf("log %q lacks %q", got, want)
	}
}
