package daemon

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/saferm"
)

// The test binary is the daemon when these are set: TestMain runs Run on
// the directory, with the idle time given, after writing its environment
// to the file given.
const (
	testDirEnv  = "CABOOSE_DAEMON_TEST_DIR"
	testIdleEnv = "CABOOSE_DAEMON_TEST_IDLE"
	testEnvFile = "CABOOSE_DAEMON_TEST_ENVFILE"
	// testNotifyEnv, when set, names the file the daemon's host
	// notifications are appended to, and its links notify when ensured
	// (notifyLinks).
	testNotifyEnv = "CABOOSE_DAEMON_TEST_NOTIFY"
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(testDirEnv); dir != "" {
		os.Exit(testDaemon(dir))
	}
	os.Exit(m.Run())
}

func testDaemon(dir string) int {
	if f := os.Getenv(testEnvFile); f != "" {
		if err := os.WriteFile(f, []byte(strings.Join(os.Environ(), "\n")), 0o600); err != nil {
			return 1
		}
	}
	idle, err := time.ParseDuration(os.Getenv(testIdleEnv))
	if err != nil {
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	opts := Options{Dir: dir, Version: "v0.0.0-test", Idle: idle, LockWait: StartWait}
	if f := os.Getenv(testNotifyEnv); f != "" {
		opts.Notifier = NewNotifier(notifyFallback(f))
		opts.Links = notifyLinks{n: opts.Notifier}
	}
	switch err := Run(ctx, opts); {
	case errors.Is(err, ErrRunning):
		return 3
	case err != nil:
		return 1
	}
	return 0
}

// harness is a temp CABOOSE_HOME, short enough for a socket, and every
// daemon a test spawned in it, all killed and reaped at cleanup.
type harness struct {
	t    *testing.T
	home string
	dir  string

	mu       sync.Mutex
	spawned  []*spawned
	extraEnv []string
}

type spawned struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home, err := os.MkdirTemp("", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, home: home, dir: Dir(home, false)}
	t.Cleanup(func() {
		h.killAll()
		if err := saferm.RemoveRoot(home); err != nil {
			t.Errorf("removing %s: %v", home, err)
		}
	})
	return h
}

func (h *harness) killAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.spawned {
		_ = s.cmd.Process.Signal(syscall.SIGKILL)
		<-s.done
	}
}

// config is a client's Config whose Spawn starts the test binary as the
// daemon, exiting after idle with no client.
func (h *harness) config(idle time.Duration) Config {
	return Config{Dir: h.dir, Version: "v0.0.0-test", Spawn: func() error {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		env := append(MinimalEnv(os.Getenv, h.home), testDirEnv+"="+h.dir, testIdleEnv+"="+idle.String())
		env = append(env, h.extraEnv...)
		cmd, err := StartDetached(exe, []string{"-test.run=^$"}, env)
		if err != nil {
			return err
		}
		s := &spawned{cmd: cmd, done: make(chan struct{})}
		go func() { _ = cmd.Wait(); close(s.done) }()
		h.mu.Lock()
		h.spawned = append(h.spawned, s)
		h.mu.Unlock()
		return nil
	}}
}

func (h *harness) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.spawned)
}

// alive is how many spawned daemons have not exited.
func (h *harness) alive() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.spawned {
		select {
		case <-s.done:
		default:
			n++
		}
	}
	return n
}

func (h *harness) ensure(cfg Config) *daemonproto.Client {
	h.t.Helper()
	c, err := Ensure(context.Background(), cfg)
	if err != nil {
		h.t.Fatalf("Ensure: %v", err)
	}
	h.t.Cleanup(func() { c.Close() })
	return c
}

func pidOfClient(t *testing.T, c *daemonproto.Client) int {
	t.Helper()
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return st.Daemon.PID
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, d)
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Commands racing to start the daemon end with one: one spawn, one PID,
// one daemon.json.
func TestRacingStartsEndWithOneDaemon(t *testing.T) {
	h := newHarness(t)
	cfg := h.config(10 * time.Second)
	const n = 8
	pids := make([]int, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := Ensure(context.Background(), cfg)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			st, err := c.Status(context.Background())
			if err != nil {
				errs <- err
				return
			}
			pids[i] = st.Daemon.PID
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for _, p := range pids {
		if p != pids[0] || p == 0 {
			t.Fatalf("pids %v: want one daemon", pids)
		}
	}
	if h.count() != 1 || h.alive() != 1 {
		t.Errorf("spawned %d, alive %d: want 1", h.count(), h.alive())
	}
	st, err := ReadState(h.dir)
	if err != nil || st.PID != pids[0] || st.Protocol != daemonproto.ProtocolVersion || st.Version != "v0.0.0-test" || st.Dir != h.dir {
		t.Errorf("daemon.json = %+v, %v; want pid %d", st, err, pids[0])
	}
	ents, _ := filepath.Glob(filepath.Join(h.dir, "*daemon.json*"))
	if len(ents) != 1 {
		t.Errorf("state files %v", ents)
	}
	for name, want := range map[string]os.FileMode{h.dir: 0o700 | os.ModeDir, Socket(h.dir): 0o600 | os.ModeSocket} {
		if fi, err := os.Lstat(name); err != nil || fi.Mode() != want {
			t.Errorf("%s: %v %v, want %v", name, fi.Mode(), err, want)
		}
	}
}

// The lock refuses a second daemon on the same directory, which leaves
// the first one's socket alone.
func TestSecondDaemonRefused(t *testing.T) {
	h := newHarness(t)
	c := h.ensure(h.config(10 * time.Second))
	first := pidOfClient(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Run(ctx, Options{Dir: h.dir, Version: "x", Idle: time.Second}); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Run: %v, want ErrRunning", err)
	}
	c2, err := Connect(context.Background(), Config{Dir: h.dir, Version: "v0.0.0-test"})
	if err != nil {
		t.Fatalf("after the refused one: %v", err)
	}
	defer c2.Close()
	if p := pidOfClient(t, c2); p != first {
		t.Errorf("pid %d, want %d", p, first)
	}
}

// The daemon stays while a client is connected, and exits once none has
// been for its idle time, removing its socket and daemon.json.
func TestIdleExit(t *testing.T) {
	h := newHarness(t)
	c, err := Ensure(context.Background(), h.config(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("with a client connected, the daemon went: %v", err)
	}
	if h.alive() != 1 {
		t.Fatal("daemon exited with a client connected")
	}
	c.Close()
	waitFor(t, 5*time.Second, "idle exit", func() bool { return h.alive() == 0 })
	if h, _ := held(filepath.Join(h.dir, LockFile)); h {
		t.Error("lock still held")
	}
	for _, f := range []string{SocketFile, StateFile} {
		if exists(filepath.Join(h.dir, f)) {
			t.Errorf("%s left behind", f)
		}
	}
}

// stop ends the daemon at once, releasing its lock and removing its
// socket; a second stop finds none.
func TestStop(t *testing.T) {
	h := newHarness(t)
	cfg := h.config(10 * time.Second)
	extra, err := Ensure(context.Background(), cfg) // a client that stays: stop does not wait for it
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	start := time.Now()
	running, err := Stop(context.Background(), cfg)
	if err != nil || !running {
		t.Fatalf("Stop: %v, %v", running, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("stop took %v", d)
	}
	if hd, _ := held(filepath.Join(h.dir, LockFile)); hd {
		t.Error("lock still held after Stop")
	}
	for _, f := range []string{SocketFile, StateFile} {
		if exists(filepath.Join(h.dir, f)) {
			t.Errorf("%s left behind", f)
		}
	}
	waitFor(t, 2*time.Second, "the process exits", func() bool { return h.alive() == 0 })
	if running, err := Stop(context.Background(), cfg); err != nil || running {
		t.Errorf("second Stop: %v, %v", running, err)
	}
	// And the next command starts another.
	c := h.ensure(cfg)
	if err := c.Ping(context.Background()); err != nil || h.count() != 2 {
		t.Errorf("restart: %v, spawned %d", err, h.count())
	}
}

// The peer check refuses another user's connection, and accepts the
// daemon's own (through the system's real credentials).
func TestPeerCheck(t *testing.T) {
	own := ownUID()
	if checkPeer(own, own) != nil || checkPeer(own+1, own) == nil {
		t.Fatal("checkPeer")
	}
	for _, peer := range []string{"own", "other", "unreadable"} {
		other := peer != "own"
		h := newHarness(t)
		opts := Options{Dir: h.dir, Version: "x", Idle: time.Minute, Log: testLog{t}}
		switch peer {
		case "other":
			opts.peer = func(net.Conn) (peerCred, error) { return peerCred{UID: own + 1}, nil }
		case "unreadable":
			opts.peer = func(net.Conn) (peerCred, error) { return peerCred{UID: own}, errors.New("no credentials") }
		}
		stop := runInProcess(t, opts)
		c, err := Connect(context.Background(), Config{Dir: h.dir, Version: "v0.0.0-test"})
		if other {
			if err == nil {
				c.Close()
				t.Errorf("%s peer's connection was served", peer)
			}
		} else if err != nil {
			t.Errorf("own connection refused: %v", err)
		} else {
			c.Close()
		}
		stop()
	}
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// runInProcess runs a daemon in this process until the returned stop, or
// the test's end.
func runInProcess(t *testing.T, opts Options) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()
	waitFor(t, 5*time.Second, "the daemon listens", func() bool {
		c, err := net.Dial("unix", Socket(opts.Dir))
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// A CABOOSE_HOME too long for the socket is refused, naming CABOOSE_HOME,
// before anything is created.
func TestSocketPathTooLong(t *testing.T) {
	home := filepath.Join(t.TempDir(), strings.Repeat("x", 120))
	dir := Dir(home, false)
	for name, err := range map[string]error{
		"Run":     Run(context.Background(), Options{Dir: dir}),
		"Connect": func() error { _, err := Connect(context.Background(), Config{Dir: dir}); return err }(),
		"Ensure": func() error {
			_, err := Ensure(context.Background(), Config{Dir: dir, Spawn: func() error { return nil }})
			return err
		}(),
	} {
		if err == nil || !strings.Contains(err.Error(), "CABOOSE_HOME") || !strings.Contains(err.Error(), home) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if exists(home) {
		t.Error("created CABOOSE_HOME")
	}
}

func TestDir(t *testing.T) {
	if d := Dir("/h", false); d != "/h/daemon" {
		t.Error(d)
	}
	if d := Dir("/h", true); d != "/h/daemon-dev" {
		t.Error(d)
	}
	if !slices.Equal(Dirs("/h"), []string{"/h/daemon", "/h/daemon-dev"}) {
		t.Error(Dirs("/h"))
	}
}

// The daemon is started with a minimal environment, not the starting
// command's.
func TestDaemonEnvironmentIsMinimal(t *testing.T) {
	h := newHarness(t)
	t.Setenv("SSH_AUTH_SOCK", "/nowhere/agent.sock")
	t.Setenv("DOCKER_HOST", "unix:///nowhere/docker.sock")
	t.Setenv("CABOOSE_DATA_DIR", "/nowhere/data")
	t.Setenv("LANG", "C.UTF-8")
	envFile := filepath.Join(h.home, "env")
	h.extraEnv = []string{testEnvFile + "=" + envFile}
	h.ensure(h.config(10 * time.Second))
	b, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	allowed := append(slices.Clone(minimalVars), "CABOOSE_HOME", testDirEnv, testIdleEnv, testEnvFile)
	got := map[string]string{}
	for _, kv := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
		if !slices.Contains(allowed, k) {
			t.Errorf("the daemon inherited %s", kv)
		}
	}
	if got["CABOOSE_HOME"] != h.home || got["LANG"] != "C.UTF-8" {
		t.Errorf("environment %v", got)
	}
}

func TestMinimalEnv(t *testing.T) {
	vars := map[string]string{"HOME": "/h", "USER": "u", "SHELL": "/bin/zsh", "SSH_AUTH_SOCK": "/s", "CABOOSE_HOME": "/other"}
	got := MinimalEnv(func(k string) string { return vars[k] }, "/h/.caboose")
	want := []string{"HOME=/h", "USER=u", "SHELL=/bin/zsh", "CABOOSE_HOME=/h/.caboose"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Asking a daemon that is not there creates nothing: not the daemon's
// directory, not CABOOSE_HOME's contents.
func TestStatusAndStopCreateNothing(t *testing.T) {
	h := newHarness(t)
	cfg := Config{Dir: h.dir, Version: "v0.0.0-test"}
	if _, err := Connect(context.Background(), cfg); daemonproto.Kind(err) != daemonproto.KindUnavailable {
		t.Errorf("Connect: %v", err)
	}
	if running, err := Stop(context.Background(), cfg); running || err != nil {
		t.Errorf("Stop: %v %v", running, err)
	}
	if ents, _ := os.ReadDir(h.home); len(ents) != 0 {
		t.Errorf("created %v", ents)
	}
	// With the directory there and no daemon, still nothing new.
	if err := os.Mkdir(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if running, err := Stop(context.Background(), cfg); running || err != nil {
		t.Errorf("Stop: %v %v", running, err)
	}
	if ents, _ := os.ReadDir(h.dir); len(ents) > 1 || len(ents) == 1 && ents[0].Name() != StartLockFile {
		t.Errorf("created %v", ents)
	}
}

// A socket no daemon listens on is left alone by a client, and replaced by
// the next daemon, which holds the lock.
func TestStaleSocket(t *testing.T) {
	h := newHarness(t)
	if err := os.Mkdir(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: Socket(h.dir), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	if _, err := Connect(context.Background(), Config{Dir: h.dir, Version: "v0.0.0-test"}); daemonproto.Kind(err) != daemonproto.KindUnavailable {
		t.Fatalf("Connect to a stale socket: %v", err)
	}
	if !exists(Socket(h.dir)) {
		t.Fatal("a client removed the socket")
	}
	c := h.ensure(h.config(10 * time.Second))
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// With no links to serve, the link requests answer unknown_method, and
// unknown methods too; ping and status work.
func TestRequests(t *testing.T) {
	h := newHarness(t)
	runInProcess(t, Options{Dir: h.dir, Version: "v1.2.3", Dev: true, Idle: time.Minute, Log: testLog{t}})
	c, err := Connect(context.Background(), Config{Dir: h.dir, Version: "v0.0.0-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.EnsureLink(context.Background(), "default"); daemonproto.Kind(err) != daemonproto.KindUnknownMethod {
		t.Errorf("ensure_link: %v", err)
	}
	if err := c.Call(context.Background(), "subscribe", nil, nil); daemonproto.Kind(err) != daemonproto.KindUnknownMethod {
		t.Errorf("subscribe: %v", err)
	}
	st, err := c.Status(context.Background())
	if err != nil || st.Daemon.Version != "v1.2.3" || !st.Daemon.Dev || st.Daemon.Dir != h.dir || st.Daemon.PID != os.Getpid() || st.Envs == nil {
		t.Errorf("status %+v, %v", st, err)
	}
}

// Ensure does not start a daemon over one that is there but silent.
func TestEnsureLeavesSilentDaemon(t *testing.T) {
	h := newHarness(t)
	if err := os.Mkdir(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: Socket(h.dir), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := h.config(10 * time.Second)
	cfg.HelloTimeout = 200 * time.Millisecond
	if _, err := Ensure(context.Background(), cfg); daemonproto.Kind(err) != daemonproto.KindTimeout {
		t.Errorf("Ensure: %v, want a timeout", err)
	}
	if h.count() != 0 {
		t.Error("spawned a daemon beside a silent one")
	}
}

// A connection past the cap is closed at once; the others are served.
func TestConnectionCap(t *testing.T) {
	old := maxConns
	maxConns = 2
	t.Cleanup(func() { maxConns = old })
	h := newHarness(t)
	runInProcess(t, Options{Dir: h.dir, Version: "x", Idle: time.Minute, Log: testLog{t}})
	cfg := Config{Dir: h.dir, Version: "v0.0.0-test"}
	// runInProcess's probes count against the cap until the daemon sees
	// them closed, so a connection may be refused at first; once two are
	// served, nothing else is open.
	var open []*daemonproto.Client
	for range 2 {
		waitFor(t, 2*time.Second, "a connection is served", func() bool {
			c, err := Connect(context.Background(), cfg)
			if err == nil {
				open = append(open, c)
			}
			return err == nil
		})
	}
	t.Cleanup(func() {
		for _, c := range open {
			c.Close()
		}
	})
	if c, err := Connect(context.Background(), cfg); err == nil {
		c.Close()
		t.Error("a connection past the cap was served")
	}
	open[0].Close()
	waitFor(t, 2*time.Second, "a slot frees", func() bool {
		c, err := Connect(context.Background(), cfg)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	open[1].Close()
}

// A daemon started while another still holds the lock waits for it, up to
// LockWait, instead of giving up.
func TestRunWaitsForTheLock(t *testing.T) {
	h := newHarness(t)
	if err := os.Mkdir(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lk, err := tryLock(filepath.Join(h.dir, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(300*time.Millisecond, func() { unlock(lk) })
	runInProcess(t, Options{Dir: h.dir, Version: "x", Idle: time.Minute, LockWait: 5 * time.Second, Log: testLog{t}})
}

// Ready runs once the daemon holds its lock and serves its socket, and
// never in a daemon refused for another holding the lock.
func TestReadyRunsOnlyInTheDaemon(t *testing.T) {
	h := newHarness(t)
	ready := make(chan *log.Logger, 1)
	runInProcess(t, Options{Dir: h.dir, Version: "x", Idle: 10 * time.Second, Log: testLog{t},
		Ready: func(l *log.Logger) {
			c, err := net.Dial("unix", Socket(h.dir))
			if err == nil {
				c.Close()
			}
			if err != nil {
				t.Errorf("Ready ran before the socket served: %v", err)
			}
			ready <- l
		}})
	select {
	case l := <-ready:
		if l == nil {
			t.Error("Ready got no log")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ready never ran")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ran := false
	if err := Run(ctx, Options{Dir: h.dir, Version: "x", Ready: func(*log.Logger) { ran = true }}); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Run: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if ran {
		t.Error("a refused daemon ran Ready")
	}
}
