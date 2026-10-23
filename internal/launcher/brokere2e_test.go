package launcher

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/policy"
	"github.com/bfreis/caboose/internal/saferm"
)

// brokerDaemonEnv makes the test binary the daemon: the real `caboose
// daemon` (runDaemon, with its login-shell environment, its socket and
// its links), but for one thing -- a broker for the environment alpha,
// opened and seeded with requests as a running link would, since no
// sandbox runs here. The binary the commands start is itself, with
// nothing in its environment but the daemon's minimal one, so no daemon
// of a test can reach the real docker or the user's home.
const brokerDaemonEnv = "CABOOSE_TEST_BROKER_DAEMON"

// recoveryGated, as brokerDaemonEnv's value, also holds the daemon's
// recovery of its links until the file recoveryGate appears in
// CABOOSE_HOME: a test's way to see the daemon while it recovers.
const (
	recoveryGated = "gated"
	recoveryGate  = "recovery-gate"
)

func brokerDaemonMain() int {
	cfg, err := config.Machine(os.Getenv)
	if err != nil {
		return 2
	}
	daemonLinksSetup = func(l *DaemonLinks) {
		b, _, err := openTestBroker(cfg.CabooseHome, "alpha", &fakeOpener{})
		if err != nil {
			os.Exit(3)
		}
		seed := []policy.Request{egressReq("api.example.com", "10.0.0.5"), egressReq("other.example.com", "10.0.0.6")}
		if _, err := b.File(broker.Ask{Request: seed[0], Context: "I need the API", Options: []string{"once", "an hour"},
			Recommendation: "once", Session: "s1"}); err != nil {
			os.Exit(4)
		}
		if _, err := b.File(broker.Ask{Request: seed[1]}); err != nil {
			os.Exit(4)
		}
		if _, err := b.Decide(urlReq("https://example.com/a", "https://example.com")); err != nil {
			os.Exit(4)
		}
		l.brokerHook = func(env string) *brokerHandle {
			if env == "alpha" {
				return &brokerHandle{b: b}
			}
			return nil
		}
		if os.Getenv(brokerDaemonEnv) == recoveryGated {
			newApp := l.newApp
			gate := filepath.Join(cfg.CabooseHome, recoveryGate)
			l.newApp = func(env string) (*App, error) {
				for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
					if _, err := os.Stat(gate); err == nil {
						break
					}
				}
				return newApp(env)
			}
		}
	}
	app := &App{Cfg: cfg, Stdout: io.Discard, Stderr: io.Discard}
	if err := app.Daemon(nil); err != nil {
		return 1
	}
	return 0
}

// e2eWorld is a CABOOSE_HOME short enough for a socket, with the
// environment alpha, and an App that starts the test binary as the daemon.
type e2eWorld struct {
	t      *testing.T
	home   string
	getenv config.Env
	out    *bytes.Buffer
	errb   *bytes.Buffer
	app    *App

	// mode is brokerDaemonEnv's value for the daemons it starts: "1",
	// or recoveryGated.
	mode string

	mu    sync.Mutex
	procs []*e2eProc
}

type e2eProc struct {
	pid  int
	done chan struct{}
}

func newE2EWorld(t *testing.T) *e2eWorld {
	t.Helper()
	home, err := os.MkdirTemp("", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	w := &e2eWorld{t: t, home: home, out: &bytes.Buffer{}, errb: &bytes.Buffer{}, mode: "1"}
	t.Cleanup(func() {
		w.stopDaemons()
		if err := saferm.RemoveRoot(home); err != nil {
			t.Errorf("removing %s: %v", home, err)
		}
	})
	// The daemon takes its PATH from a login shell: this one gives it a
	// docker that is nothing, first.
	fakeBin := filepath.Join(home, "fakebin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "docker"), []byte("#!/bin/sh\necho 'the test reached docker' >&2\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(home, "login-shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\necho 'PATH="+fakeBin+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"HOME": home, "CABOOSE_HOME": home, "SHELL": shell, "USER": "tester", "LOGNAME": "tester", "CABOOSE_NO_AUTO_UPDATE": "1"}
	w.getenv = func(k string) string { return vars[k] }
	envDir := config.EnvDirFor(home, "alpha")
	if err := os.MkdirAll(filepath.Join(envDir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, config.FileName), []byte(brokerConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	w.app = w.appFor("alpha")
	return w
}

func (w *e2eWorld) appFor(env string) *App {
	cfg, err := config.Load(w.getenv, config.OSFS{}, env)
	if err != nil {
		w.t.Fatal(err)
	}
	return &App{Cfg: cfg, Stdout: w.out, Stderr: w.errb,
		Executable: func() (string, error) { return os.Executable() },
		Spawn: func(exe string, _ ...string) error {
			cmd, err := daemon.StartDetached(exe, []string{"-test.run=^$"}, append(daemon.MinimalEnv(w.getenv, w.home), brokerDaemonEnv+"="+w.mode))
			if err != nil {
				return err
			}
			p := &e2eProc{pid: cmd.Process.Pid, done: make(chan struct{})}
			go func() { _ = cmd.Wait(); close(p.done) }()
			w.mu.Lock()
			w.procs = append(w.procs, p)
			w.mu.Unlock()
			return nil
		}}
}

// stopDaemons ends every daemon the commands started, and reaps it.
func (w *e2eWorld) stopDaemons() {
	w.mu.Lock()
	procs := w.procs
	w.mu.Unlock()
	for _, p := range procs {
		_ = syscall.Kill(p.pid, syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(p.pid, syscall.SIGKILL)
			<-p.done
		}
	}
}

func (w *e2eWorld) reset() { w.out.Reset(); w.errb.Reset() }

func (w *e2eWorld) config() string {
	b, err := os.ReadFile(filepath.Join(config.EnvDirFor(w.home, "alpha"), config.FileName))
	if err != nil {
		w.t.Fatal(err)
	}
	return string(b)
}

// The commands against a real daemon over its real socket: it is started
// by the first of them, and answers for the seeded broker.
func TestRequestCommandsAgainstARealDaemon(t *testing.T) {
	w := newE2EWorld(t)
	a := w.app

	if err := a.Requests(nil); err != nil {
		t.Fatalf("requests: %v\n%s", err, w.errb)
	}
	s := w.out.String()
	for _, want := range []string{"#1", "#2", "#3", "api.example.com", "the sandbox says:", "I need the API", "other.example.com", "https://example.com"} {
		if !strings.Contains(s, want) {
			t.Errorf("the listing lacks %q:\n%s", want, s)
		}
	}
	if len(w.procs) != 1 {
		t.Errorf("%d daemons started", len(w.procs))
	}
	w.reset()
	if err := a.Requests([]string{"show", "1"}); err != nil || !strings.Contains(w.out.String(), "session (as the sandbox names it): s1") {
		t.Errorf("show: %v\n%s", err, w.out)
	}

	w.reset()
	if err := a.Permissions(); err != nil || !strings.Contains(w.out.String(), "deploy: /usr/bin/true now") || !strings.Contains(w.out.String(), "vm (profile vm.default)") {
		t.Errorf("permissions: %v\n%s", err, w.out)
	}

	// For hours: a grant, and it is listed and revoked.
	w.reset()
	if err := a.Requests([]string{"approve", "1", "--for", "2h"}); err != nil || !strings.Contains(w.out.String(), "Request #1 allowed (hours 2h)") || !strings.Contains(w.out.String(), "grant g1") {
		t.Fatalf("approve: %v\n%s", err, w.out)
	}
	w.reset()
	if err := a.Grants(nil); err != nil || !strings.Contains(w.out.String(), "g1") || !strings.Contains(w.out.String(), "egress:api.example.com") {
		t.Errorf("grants: %v\n%s", err, w.out)
	}
	w.reset()
	if err := a.Requests([]string{"approve", "1"}); err == nil || !strings.Contains(err.Error(), "already answered") {
		t.Errorf("a second answer: %v", err)
	}
	if err := a.Grants([]string{"revoke", "g1"}); err != nil {
		t.Errorf("revoke: %v", err)
	}
	if err := a.Grants([]string{"revoke", "g1"}); err == nil {
		t.Error("revoked twice")
	}

	// Deny.
	w.reset()
	if err := a.Requests([]string{"deny", "3"}); err != nil || !strings.Contains(w.out.String(), "Request #3 denied.") {
		t.Errorf("deny: %v\n%s", err, w.out)
	}

	// Always: the literal change is shown, a typed yes writes it.
	before := w.config()
	w.reset()
	a.Terminal = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("yes\n")), nil }
	if err := a.Requests([]string{"approve", "2", "--always"}); err != nil {
		t.Fatalf("always: %v\n%s\n%s", err, w.out, w.errb)
	}
	if !strings.Contains(w.out.String(), `+ egress_allow = ["*.corp.example", "other.example.com"]`) || !strings.Contains(w.out.String(), "allowed (always)") {
		t.Errorf("always output:\n%s", w.out)
	}
	want := strings.Replace(before, `egress_allow = ["*.corp.example"]`, `egress_allow = ["*.corp.example", "other.example.com"]`, 1)
	if got := w.config(); got != want {
		t.Errorf("config.toml:\n%s\nwant\n%s", got, want)
	}
	w.reset()
	if err := a.Requests(nil); err != nil || !strings.Contains(w.out.String(), "no pending requests") {
		t.Errorf("pending now: %v\n%s", err, w.out)
	}

	// An environment this daemon runs no link for.
	beta := w.appFor("beta")
	if err := beta.Requests(nil); err == nil || !strings.Contains(err.Error(), "no link of this daemon serves environment beta") {
		t.Errorf("beta: %v", err)
	}
	if len(w.procs) != 1 {
		t.Errorf("%d daemons started", len(w.procs))
	}
}

// Over the real socket, no client may say it is the daemon: the hello is
// refused before any request. The command line's answer is recorded as
// the command line's.
func TestRealDaemonRefusesOtherClientsAnswers(t *testing.T) {
	w := newE2EWorld(t)
	if err := w.app.Requests(nil); err != nil { // starts the daemon
		t.Fatal(err)
	}
	for _, kind := range []string{daemonproto.ClientDaemon} {
		c, err := daemonproto.Dial(context.Background(), daemon.Socket(w.app.daemonDir()), daemonproto.Options{Client: kind, Version: "v1"})
		if err == nil {
			c.Close()
		}
		if daemonproto.Kind(err) != daemonproto.KindBadRequest {
			t.Errorf("%s hello: %v", kind, err)
		}
	}
	c, err := daemonproto.Dial(context.Background(), daemon.Socket(w.app.daemonDir()), daemonproto.Options{Client: daemonproto.ClientCLI, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Requests(context.Background(), "alpha", false, 0)
	if err != nil || len(r.Requests) != 3 {
		t.Errorf("cli reads: %v %v", r, err)
	}
	a, err := c.Answer(context.Background(), daemonproto.AnswerParams{Env: "alpha", ID: 1, Verdict: "deny"})
	if err != nil || a.Request.DecidedBy != daemonproto.ClientCLI {
		t.Errorf("cli answered: %+v %v", a, err)
	}
	c.Close()
	var de *daemonproto.Error
	w.reset()
	if err := w.app.Requests([]string{"show", "1"}); err != nil || errors.As(err, &de) || !strings.Contains(w.out.String(), "#1") {
		t.Errorf("%v\n%s", err, w.out)
	}
}

// A daemon that starts says it is recovering from before it answers
// anyone until its links are back, and caboose daemon restart returns
// only once they are: a status or doctor right after it finds them.
func TestDaemonRestartWaitsForRecovery(t *testing.T) {
	w := newE2EWorld(t)
	w.mode = recoveryGated
	// The gate opens a moment after a status has said the daemon is
	// recovering: never, if no status says so.
	seen := make(chan bool, 1)
	go func() {
		watcher := w.appFor("alpha")
		recovering := false
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && !recovering; time.Sleep(10 * time.Millisecond) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if c, err := watcher.dialDaemon(ctx, false); err == nil {
				if st, err := c.Status(ctx); err == nil && st.Daemon.Recovering {
					recovering = true
				}
				c.Close()
			}
			cancel()
		}
		if recovering {
			time.Sleep(300 * time.Millisecond)
		}
		if err := os.WriteFile(filepath.Join(w.home, recoveryGate), nil, 0o600); err != nil {
			t.Error(err)
		}
		seen <- recovering
	}()
	err := w.app.Daemon([]string{"restart"})
	if !<-seen {
		t.Error("no status said the daemon was recovering")
	}
	if err != nil {
		t.Fatalf("restart: %v\n%s", err, w.errb)
	}
	if s := w.out.String(); !strings.Contains(s, "caboose daemon: restarted (pid ") || strings.Contains(s, "still bringing back") {
		t.Errorf("restart said %q", s)
	}
}
