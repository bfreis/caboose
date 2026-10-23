package launcher

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/cappedlog"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/hostlink"
)

// jobsWorld is a boxApp's environment served by the daemon's links: each
// App the links make is a copy of the boxApp's, on the same in-memory
// sandbox.
type jobsWorld struct {
	b     *boxApp
	l     *DaemonLinks
	clock time.Time

	mu    sync.Mutex
	notes []string // "title|text" of every notification
}

func newJobsWorld(t *testing.T, iso string) *jobsWorld {
	t.Helper()
	b := newBoxApp(t, iso, runningBox(iso))
	b.Cfg.CabooseHome = b.tmp
	b.Cfg.EnvDir = filepath.Join(b.tmp, "envs", b.Cfg.Env)
	b.Cfg.OpenURLs = hostlink.OpenOff
	b.Cfg.SSHAgent, b.Cfg.ForwardPorts = "none", "none"
	if err := os.MkdirAll(b.Cfg.EnvDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// What the link's request broker reads, as the daemon's links do.
	vars := map[string]string{"HOME": b.tmp, "CABOOSE_HOME": b.tmp}
	b.Cfg.Getenv = func(k string) string { return vars[k] }
	body := "[link]\nforward_ports = []\nopen_urls = \"off\"\nssh_agent = \"none\"\n"
	if err := os.WriteFile(filepath.Join(b.Cfg.EnvDir, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b.data, 0o700); err != nil {
		t.Fatal(err)
	}
	w := &jobsWorld{b: b, clock: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	w.l = NewDaemonLinks(DaemonLinksOptions{Dir: filepath.Join(b.tmp, "daemon"), CabooseHome: b.tmp,
		NewApp: func(env string) (*App, error) {
			if env != b.Cfg.Env {
				return nil, daemonproto.NewError(daemonproto.KindUnknownEnv, "no environment %s", env)
			}
			a := *b.App
			return &a, nil
		},
		JobNotify: func(env string) func(title, text string) {
			return func(title, text string) {
				w.mu.Lock()
				w.notes = append(w.notes, title+"|"+text)
				w.mu.Unlock()
			}
		}})
	w.l.jobNow = func() time.Time {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.clock
	}
	t.Cleanup(w.l.Close)
	return w
}

func (w *jobsWorld) notified() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.notes)
}

// wait waits for env's latest job to end, and returns it.
func (w *jobsWorld) wait(t *testing.T, env string) daemonproto.JobInfo {
	t.Helper()
	e := w.l.env(env)
	e.mu.Lock()
	j := e.job
	e.mu.Unlock()
	if j == nil {
		t.Fatal("no job")
	}
	select {
	case <-j.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the job never ended")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return j.info
}

func (w *jobsWorld) status(t *testing.T) daemonproto.EnvStatus {
	t.Helper()
	for _, st := range w.l.Envs() {
		if st.Name == w.b.Cfg.Env {
			if err := (&daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{st}}).Validate(); err != nil {
				t.Fatalf("status: %v", err)
			}
			return st
		}
	}
	t.Fatalf("no status for %s: %+v", w.b.Cfg.Env, w.l.Envs())
	return daemonproto.EnvStatus{}
}

func (w *jobsWorld) jobsLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.b.Cfg.EnvDir, jobsLogFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runJob starts a job for env that runs run on the environment's App,
// as the daemon's jobs do (startJob, endJob), and returns it, running.
func (w *jobsWorld) runJob(env string, run func(a *App) error) (*daemonproto.JobInfo, error) {
	e, err := w.l.jobEnv(env)
	if err != nil {
		return nil, err
	}
	app, err := w.l.newApp(env)
	if err != nil {
		return nil, err
	}
	info := daemonproto.JobInfo{Kind: daemonproto.JobApply, Proposal: "test.toml"}
	return w.l.startJob(e, app, info, func(j *envJob, out *cappedlog.Log) {
		lg := log.New(out, "", log.LstdFlags)
		jobOutput(app, out)
		err := run(app)
		w.l.endJob(e, j, out, lg, err, nil, env+": done")
	})
}

// jobs.log is capped: past its size it is moved aside, as link.log is.
func TestDaemonJobsLogCapped(t *testing.T) {
	w := newJobsWorld(t, isolationContainer)
	w.l.jobsLogMax = 400
	for range 6 {
		if _, err := w.runJob("default", func(a *App) error {
			a.Note("%s", strings.Repeat("x", 100))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		w.wait(t, "default")
	}
	for _, name := range []string{jobsLogFile, jobsLogFile + ".1"} {
		fi, err := os.Stat(filepath.Join(w.b.Cfg.EnvDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > 400 || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %d bytes, mode %v", name, fi.Size(), fi.Mode())
		}
	}
}

// caboose daemon status says how an environment's job is.
func TestDaemonStatusJobLine(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		job  *daemonproto.JobInfo
		want string
	}{
		{nil, "  env alpha: sandbox running, link running"},
		{&daemonproto.JobInfo{Kind: daemonproto.JobApply, State: daemonproto.JobRunning, Since: at, Proposal: "a.toml"}, "  env alpha: sandbox running, link running, apply job running"},
		{&daemonproto.JobInfo{Kind: daemonproto.JobApply, State: daemonproto.JobFailed, Since: at, Finished: at, Proposal: "a.toml", Error: "no"}, "  env alpha: sandbox running, link running, apply job failed (no)"},
	} {
		e := daemonproto.EnvStatus{Name: "alpha", Sandbox: daemonproto.SandboxRunning, Link: daemonproto.LinkRunning, Job: tc.job}
		if got := daemonEnvLine(e); got != tc.want {
			t.Errorf("%q, want %q", got, tc.want)
		}
	}
}

// A failed job's error, however long or wherever it came from, reaches
// status as one printable, clipped line that status validates, and is
// notified with it.
func TestDaemonJobFailure(t *testing.T) {
	w := newJobsWorld(t, isolationContainer)
	long := "first\nsecond\x1b[31m\u202e" + strings.Repeat("x", 3*daemonproto.MaxJobError)
	if _, err := w.runJob("default", func(*App) error { return errors.New(long) }); err != nil {
		t.Fatal(err)
	}
	j := w.wait(t, "default")
	if j.State != daemonproto.JobFailed || len(j.Error) > daemonproto.MaxJobError ||
		!strings.HasPrefix(j.Error, "first second?[31m?xxx") || !strings.HasSuffix(j.Error, "...") {
		t.Fatalf("ended %+v", j)
	}
	st := w.status(t)
	if st.Job == nil || st.Job.Error != j.Error {
		t.Errorf("after: %+v", st)
	}
	if nt := w.notified(); len(nt) != 1 || !strings.HasSuffix(nt[0], j.Error) {
		t.Errorf("notified %q", nt)
	}
	if lg := w.jobsLog(t); !strings.Contains(lg, "apply default: failed") {
		t.Errorf("jobs.log: %q", lg)
	}
}

// A replace waits for a running job as for a host command: the drain
// finds the job in while it runs, and holding keeps a new job out; once
// the job has ended the drain closes the gate, and no job starts after.
func TestDaemonJobHoldsReplace(t *testing.T) {
	w := newJobsWorld(t, isolationContainer)
	running, release := make(chan struct{}), make(chan struct{})
	if _, err := w.runJob("default", func(*App) error {
		close(running)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-running
	if w.l.DrainHostExecs(true) {
		t.Fatal("drained with a job running")
	}
	if n := w.l.HostExecs(); n != 1 {
		t.Errorf("a replace waits for %d, want the job", n)
	}
	close(release)
	if j := w.wait(t, "default"); j.State != daemonproto.JobDone {
		t.Fatalf("ended %+v", j)
	}
	if !w.l.DrainHostExecs(true) {
		t.Fatal("not drained once the job ended")
	}
	if _, err := w.runJob("default", func(*App) error { return nil }); daemonproto.Kind(err) != daemonproto.KindBusy {
		t.Errorf("a job after the drain: %v", err)
	}
}

// A drain that holds keeps a new job out while it waits, and one that
// lets go admits it again.
func TestDaemonJobRefusedWhileHeld(t *testing.T) {
	w := newJobsWorld(t, isolationContainer)
	if !w.l.gate.Enter() { // a host command in flight
		t.Fatal("gate closed")
	}
	if w.l.DrainHostExecs(true) {
		t.Fatal("drained with a host command in")
	}
	if _, err := w.runJob("default", func(*App) error { return nil }); daemonproto.Kind(err) != daemonproto.KindBusy {
		t.Errorf("a job while held: %v", err)
	}
	if w.l.DrainHostExecs(false) {
		t.Fatal("drained with a host command in")
	}
	if _, err := w.runJob("default", func(*App) error { return nil }); err != nil {
		t.Fatalf("a job once let go: %v", err)
	}
	w.wait(t, "default")
	w.l.gate.Leave()
}
