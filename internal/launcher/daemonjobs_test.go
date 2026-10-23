package launcher

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/hostlink"
)

// jobsWorld is a boxApp's environment served by the daemon's links: each
// App the links make is a copy of the boxApp's, on the same in-memory
// sandbox, whose link an agent in this process answers.
type jobsWorld struct {
	b     *boxApp
	box   *agentBox
	l     *DaemonLinks
	clock time.Time

	mu    sync.Mutex
	notes []string // "title|text" of every notification
}

func newJobsWorld(t *testing.T, iso string) *jobsWorld {
	t.Helper()
	fake := runningBox(iso)
	// Live sessions, which a job ends without asking: the caller asked.
	fake.Exec = func(s backend.ExecSpec) *exec.Cmd {
		if len(s.Argv) > 1 && s.Argv[0] == "tmux" && s.Argv[1] == "list-sessions" {
			return backendtest.Reply("proj-abc123\n", "", 0)
		}
		return nil
	}
	b := newBoxApp(t, iso, fake)
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
	box := &agentBox{Fake: fake, sessions: make(chan *agentproto.Session, 16)}
	b.Backend = box
	w := &jobsWorld{b: b, box: box, clock: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
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

func (w *jobsWorld) advance(d time.Duration) {
	w.mu.Lock()
	w.clock = w.clock.Add(d)
	w.mu.Unlock()
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

// waitCreate waits until the job reaches the sandbox's creation
// (inCreate closed), failing if it ends first or takes too long.
func (w *jobsWorld) waitCreate(t *testing.T, inCreate <-chan struct{}) {
	t.Helper()
	e := w.l.env(w.b.Cfg.Env)
	e.mu.Lock()
	j := e.job
	e.mu.Unlock()
	if j == nil {
		t.Fatal("no job")
	}
	select {
	case <-inCreate:
	case <-j.done:
		e.mu.Lock()
		info := j.info
		e.mu.Unlock()
		t.Fatalf("the job ended before it created the sandbox: %+v\n%s", info, w.jobsLog(t))
	case <-time.After(30 * time.Second):
		t.Fatal("the job never created the sandbox")
	}
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

// sandbox_restart's job answers at once, running, while the sandbox is
// being recreated; a second job for the environment is busy then; it ends
// done, with the sandbox recreated over its live sessions without a
// question, its notes in jobs.log, the end notified, and the link back on
// the daemon's own links -- never by dialling the daemon. Status shows the
// job for ten minutes after it ended.
func TestDaemonRestartJob(t *testing.T) {
	for _, iso := range []string{isolationContainer, isolationVM} {
		t.Run(iso, func(t *testing.T) {
			w := newJobsWorld(t, iso)
			inCreate, release := make(chan struct{}), make(chan struct{})
			w.box.OnCreate = func(backend.Spec) error {
				close(inCreate)
				<-release
				return nil
			}
			j, err := w.l.StartJob(t.Context(), daemonproto.JobRestart, "default")
			if err != nil {
				t.Fatal(err)
			}
			if j.Kind != daemonproto.JobRestart || j.State != daemonproto.JobRunning || !j.Since.Equal(w.clock) || j.Validate() != nil {
				t.Fatalf("started %+v", j)
			}
			w.waitCreate(t, inCreate)
			if _, err := w.l.StartJob(t.Context(), daemonproto.JobStop, "default"); daemonproto.Kind(err) != daemonproto.KindBusy {
				t.Errorf("a second job while one runs: %v", err)
			}
			if st := w.status(t); st.Job == nil || st.Job.State != daemonproto.JobRunning || !w.l.Busy() {
				t.Errorf("while running: %+v, busy %v", st.Job, w.l.Busy())
			}
			w.advance(time.Minute)
			close(release)
			done := w.wait(t, "default")
			if done.State != daemonproto.JobDone || !done.Finished.Equal(w.clock) || done.Error != "" {
				t.Fatalf("ended %+v", done)
			}
			if !slices.Equal(w.box.Calls, []string{"remove", "create"}) {
				t.Errorf("calls %q", w.box.Calls)
			}
			log := w.jobsLog(t)
			for _, want := range []string{"restart default: started\n", "caboose: " + w.b.noun() + " removed\n",
				"caboose: " + w.b.noun() + " recreated\n", "restart default: done\n"} {
				if !strings.Contains(log, want) {
					t.Errorf("jobs.log lacks %q:\n%s", want, log)
				}
			}
			if strings.Contains(log, "[y/N]") {
				t.Errorf("a job asked:\n%s", log)
			}
			if n := w.notified(); !slices.Equal(n, []string{"caboose: default|default restarted"}) {
				t.Errorf("notified %q", n)
			}
			// The link: the daemon's own, never a dial to its socket.
			w.box.session(t)
			if st := w.status(t); st.Link != daemonproto.LinkRunning || st.Job == nil || st.Job.State != daemonproto.JobDone {
				t.Errorf("after: %+v", st)
			}
			if len(w.b.daemon.dials) != 0 {
				t.Errorf("the job dialled the daemon: %v", w.b.daemon.dials)
			}
			w.advance(daemonproto.JobKept)
			if st := w.status(t); st.Job == nil {
				t.Error("the job left status before ten minutes")
			}
			w.advance(time.Second)
			if st := w.status(t); st.Job != nil {
				t.Errorf("the job is still shown: %+v", st.Job)
			}
		})
	}
}

// sandbox_stop's job stops the sandbox, and its link with it.
func TestDaemonStopJob(t *testing.T) {
	w := newJobsWorld(t, isolationContainer)
	if r, err := w.l.Serve(t.Context(), daemonproto.MethodEnsureLink, "default"); err != nil || r.Action != daemonproto.ActionStarted {
		t.Fatalf("ensure: %+v, %v", r, err)
	}
	w.box.session(t)
	if _, err := w.l.StartJob(t.Context(), daemonproto.JobStop, "default"); err != nil {
		t.Fatal(err)
	}
	if j := w.wait(t, "default"); j.State != daemonproto.JobDone {
		t.Fatalf("ended %+v", j)
	}
	if !slices.Equal(w.box.Calls, []string{"stop"}) || w.box.State() != "exited" {
		t.Errorf("calls %q, state %s", w.box.Calls, w.box.State())
	}
	if st := w.status(t); st.Link != daemonproto.LinkStopped || st.Sandbox != daemonproto.SandboxStopped {
		t.Errorf("after: %+v", st)
	}
	if n := w.notified(); !slices.Equal(n, []string{"caboose: default|default stopped"}) {
		t.Errorf("notified %q", n)
	}
	if w.l.Busy() {
		t.Error("busy with no job and no link")
	}
}

// A job asks nothing: any question but the session loss its caller
// confirmed fails it, with the command to run on a terminal; a failure
// is notified with its reason, and the job's sandbox is left as it was.
func TestDaemonJobNeedsNoTerminal(t *testing.T) {
	w := newJobsWorld(t, isolationContainer)
	w.l.jobAction = func(a *App, kind string) error {
		_, _, err := a.askYes("rebuild?")
		return err
	}
	if _, err := w.l.StartJob(t.Context(), daemonproto.JobRestart, "default"); err != nil {
		t.Fatal(err)
	}
	const msg = "this needs a terminal: run `caboose -e default restart`"
	if j := w.wait(t, "default"); j.State != daemonproto.JobFailed || j.Error != msg {
		t.Fatalf("ended %+v", j)
	}
	if n := w.notified(); !slices.Equal(n, []string{"caboose: default|restarting default failed: " + msg}) {
		t.Errorf("notified %q", n)
	}
	// Setup's questions too.
	w.l.jobAction = func(a *App, kind string) error {
		_, err := a.openTerminal()
		return err
	}
	if _, err := w.l.StartJob(t.Context(), daemonproto.JobStop, "default"); err != nil {
		t.Fatal(err)
	}
	if j := w.wait(t, "default"); j.Error != "this needs a terminal: run `caboose -e default stop`" {
		t.Fatalf("ended %+v", j)
	}
	// Even the session loss, were it not confirmed.
	w.l.jobAction = func(a *App, kind string) error {
		a.lossConfirmed = false
		return a.Stop()
	}
	if _, err := w.l.StartJob(t.Context(), daemonproto.JobStop, "default"); err != nil {
		t.Fatal(err)
	}
	if j := w.wait(t, "default"); j.Error != "this needs a terminal: run `caboose -e default stop`" {
		t.Fatalf("ended %+v", j)
	}
	if len(w.box.Calls) != 0 || w.box.State() != "running" {
		t.Errorf("calls %q, state %s", w.box.Calls, w.box.State())
	}
	// A panic is the job's failure.
	w.l.jobAction = func(*App, string) error { panic("boom") }
	if _, err := w.l.StartJob(t.Context(), daemonproto.JobStop, "default"); err != nil {
		t.Fatal(err)
	}
	if j := w.wait(t, "default"); j.State != daemonproto.JobFailed || j.Error != "internal error: boom" {
		t.Fatalf("ended %+v", j)
	}
}

// jobs.log is capped: past its size it is moved aside, as link.log is.
func TestDaemonJobsLogCapped(t *testing.T) {
	w := newJobsWorld(t, isolationContainer)
	w.l.jobsLogMax = 400
	w.l.jobAction = func(a *App, kind string) error {
		a.Note("%s", strings.Repeat("x", 100))
		return nil
	}
	for range 6 {
		if _, err := w.l.StartJob(t.Context(), daemonproto.JobStop, "default"); err != nil {
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

// What a job is asked for is checked: an environment, a kind.
func TestDaemonJobRefusals(t *testing.T) {
	w := newJobsWorld(t, isolationContainer)
	for _, tc := range []struct{ kind, env, want string }{
		{daemonproto.JobStop, "nosuch", daemonproto.KindUnknownEnv},
		{daemonproto.JobStop, "../x", daemonproto.KindBadRequest},
		{"start", "default", daemonproto.KindBadRequest},
	} {
		if _, err := w.l.StartJob(t.Context(), tc.kind, tc.env); daemonproto.Kind(err) != tc.want {
			t.Errorf("%s %s: %v", tc.kind, tc.env, err)
		}
	}
	if len(w.box.Calls) != 0 {
		t.Errorf("calls %q", w.box.Calls)
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
		{&daemonproto.JobInfo{Kind: daemonproto.JobRestart, State: daemonproto.JobRunning, Since: at}, "  env alpha: sandbox running, link running, restart job running"},
		{&daemonproto.JobInfo{Kind: daemonproto.JobStop, State: daemonproto.JobFailed, Since: at, Finished: at, Error: "no"}, "  env alpha: sandbox running, link running, stop job failed (no)"},
	} {
		e := daemonproto.EnvStatus{Name: "alpha", Sandbox: daemonproto.SandboxRunning, Link: daemonproto.LinkRunning, Job: tc.job}
		if got := daemonEnvLine(e); got != tc.want {
			t.Errorf("%q, want %q", got, tc.want)
		}
	}
}

// A failed job leaves a link that runs as it is, and its error, however
// long or wherever it came from, reaches status as one printable,
// clipped line that status validates.
func TestDaemonJobFailureKeepsLink(t *testing.T) {
	w := newJobsWorld(t, isolationContainer)
	if r, err := w.l.Serve(t.Context(), daemonproto.MethodEnsureLink, "default"); err != nil || r.Action != daemonproto.ActionStarted {
		t.Fatalf("ensure: %+v, %v", r, err)
	}
	w.box.session(t)
	long := "first\nsecond\x1b[31m\u202e" + strings.Repeat("x", 3*daemonproto.MaxJobError)
	w.l.jobAction = func(*App, string) error { return errors.New(long) }
	if _, err := w.l.StartJob(t.Context(), daemonproto.JobRestart, "default"); err != nil {
		t.Fatal(err)
	}
	j := w.wait(t, "default")
	if j.State != daemonproto.JobFailed || len(j.Error) > daemonproto.MaxJobError ||
		!strings.HasPrefix(j.Error, "first second?[31m?xxx") || !strings.HasSuffix(j.Error, "...") {
		t.Fatalf("ended %+v", j)
	}
	st := w.status(t)
	if st.Link != daemonproto.LinkRunning || st.Job == nil || st.Job.Error != j.Error {
		t.Errorf("after: %+v", st)
	}
	if lg := w.jobsLog(t); !strings.Contains(lg, "the link: "+daemonproto.ActionUnchanged) {
		t.Errorf("jobs.log: %q", lg)
	}
}

// A replace waits for a running job as for a host command: the drain
// finds the job in while it runs, and holding keeps a new job out; once
// the job has ended the drain closes the gate, and no job starts after.
func TestDaemonJobHoldsReplace(t *testing.T) {
	w := newJobsWorld(t, isolationContainer)
	inCreate, release := make(chan struct{}), make(chan struct{})
	w.box.OnCreate = func(backend.Spec) error {
		close(inCreate)
		<-release
		return nil
	}
	if _, err := w.l.StartJob(t.Context(), daemonproto.JobRestart, "default"); err != nil {
		t.Fatal(err)
	}
	w.waitCreate(t, inCreate)
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
	if _, err := w.l.StartJob(t.Context(), daemonproto.JobStop, "default"); daemonproto.Kind(err) != daemonproto.KindBusy {
		t.Errorf("a job after the drain: %v", err)
	}
	if calls := len(w.box.Calls); calls != 2 {
		t.Errorf("calls %q", w.box.Calls)
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
	if _, err := w.l.StartJob(t.Context(), daemonproto.JobStop, "default"); daemonproto.Kind(err) != daemonproto.KindBusy {
		t.Errorf("a job while held: %v", err)
	}
	if w.l.DrainHostExecs(false) {
		t.Fatal("drained with a host command in")
	}
	if _, err := w.l.StartJob(t.Context(), daemonproto.JobStop, "default"); err != nil {
		t.Fatalf("a job once let go: %v", err)
	}
	w.wait(t, "default")
	w.l.gate.Leave()
}
