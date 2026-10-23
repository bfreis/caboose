package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/hostlink"
)

// DaemonLinks are the daemon's links: one per environment, each a
// linkRunner and its watchers in this process, on an App of its own built
// from the environment's config.toml, holding the environment's link.lock
// as a detached helper would. Operations on one environment run one at a
// time; operations on different environments run side by side.
type DaemonLinks struct {
	dir     string // this daemon's directory, which link.json names
	dev     bool   // a checkout's build: it says nothing of a newer release
	home    string // CABOOSE_HOME, whose envs/ recovery looks through
	newApp  func(env string) (*App, error)
	decide  func(env string) hostlink.Decider
	actions func(env string) hostlink.Actions
	gate    *hostlink.ExecGate // admits every link's host-exec requests
	dialogs *dialogQueue       // every link's URL dialogs, which the attached app may take
	// brokerHook, when set, stands in for the running links' brokers
	// (brokerOf): the tests'.
	brokerHook func(env string) *brokerHandle

	ctx    context.Context // ends at Close, and every link with it
	cancel context.CancelFunc

	// recovering is set from WillRecover until Recover has looked at
	// every environment.
	recovering atomic.Bool

	// cp turns on the checkpoint schedules, nil off; cpGate lets one
	// checkpoint run at a time across them.
	cp      *CheckpointOptions
	cpGate  *checkpoint.Gate
	schedWG sync.WaitGroup

	// detailsFlights and doctorFlights run one env_details and one
	// doctor per environment at a time.
	detailsFlights flights[*daemonproto.EnvDetailsResult]
	doctorFlights  flights[*daemonproto.DoctorResult]

	// proposalMu holds, per environment, the lock its proposals are
	// deleted and applied under; under mu.
	proposalMu map[string]*sync.Mutex

	// jobNotify is where a job's end is told (daemonjobs.go); nil tells
	// no one. jobAction, when set, stands in for the job's Stop or
	// Restart: the tests'. jobsLogMax caps jobs.log; 0 is maxJobsLog.
	// jobNow is the jobs' clock; nil is time.Now.
	jobNotify  func(env string) func(title, text string)
	jobAction  func(app *App, kind string) error
	jobsLogMax int64
	jobNow     func() time.Time

	mu     sync.Mutex
	log    *log.Logger
	envs   map[string]*envLink
	scheds map[string]*schedRun // under mu
	closed bool
}

// DaemonLinksOptions configure NewDaemonLinks.
type DaemonLinksOptions struct {
	// Dir is the daemon's directory, Dev whether it is a checkout's build.
	Dir string
	Dev bool
	// CabooseHome holds the environments (envs/).
	CabooseHome string
	// NewApp is the App an environment's link runs on: its config.toml
	// loaded with the daemon's own environment (DaemonApp).
	NewApp func(env string) (*App, error)
	// Decide, when set, is what makes an environment's link's decisions;
	// nil decides as its config.toml says.
	Decide func(env string) hostlink.Decider
	// Actions, when set, are what an environment's link opens URLs,
	// notifies and asks with; nil is hostlink.System's.
	Actions func(env string) hostlink.Actions
	// Checkpoints, when set, runs each environment's checkpoint schedule
	// while this daemon serves it; nil runs none.
	Checkpoints *CheckpointOptions
	// JobNotify, when set, is where the end of an environment's stop or
	// restart job, or of an apply job, is told: the daemon's Notifier.
	// nil tells no one.
	JobNotify func(env string) func(title, text string)
}

// envLink is one environment's link, as the daemon knows it.
type envLink struct {
	name string
	op   sync.Mutex // held for the whole of an operation on the environment

	mu      sync.Mutex
	cur     *linkInstance // the running link, nil when none runs
	sandbox string        // daemonproto's Sandbox*, as last seen
	state   string        // daemonproto's Link*; "" before any operation
	since   time.Time
	lastErr string
	job     *envJob          // the latest job, nil before any
	now     func() time.Time // the jobs' clock; nil is time.Now
	// cpLimits bound the checkpoints its sandbox asks for, across its
	// links' restarts; made at the first link that offers them.
	cpLimits *hostlink.CheckpointLimits
}

// linkInstance is one run of a link.
type linkInstance struct {
	runner *linkRunner
	execs  *hostlink.Execs
	cancel context.CancelFunc
	done   chan struct{} // closed once it has stopped and let go of link.lock
	lock   *os.File      // link.lock, held while it runs

	lockMu   sync.Mutex // held while released changes, and while lock is looked at
	released bool       // link.lock has been let go of, or is being
}

var _ daemon.Links = (*DaemonLinks)(nil)

// NewDaemonLinks is the daemon's links, none running yet.
func NewDaemonLinks(o DaemonLinksOptions) *DaemonLinks {
	l := &DaemonLinks{dir: filepath.Clean(o.Dir), dev: o.Dev, home: o.CabooseHome, newApp: o.NewApp, decide: o.Decide, actions: o.Actions,
		gate: hostlink.NewExecGate(), dialogs: newDialogQueue(), log: log.New(io.Discard, "", 0), envs: map[string]*envLink{},
		cp: o.Checkpoints, cpGate: checkpoint.NewGate(), scheds: map[string]*schedRun{}, jobNotify: o.JobNotify}
	l.ctx, l.cancel = context.WithCancel(context.Background())
	return l
}

// Dialogs is the links' URL dialog queue, which the daemon hands the
// attached menu-bar app's polls (daemon.Options.Dialogs).
func (l *DaemonLinks) Dialogs() daemon.Dialogs { return l.dialogs }

// DaemonGetenv is the environment the daemon's links read their
// configuration with: the daemon's own, without the variables that are a
// command's and never the daemon's. CABOOSE_DATA_DIR would put every
// environment on one data dir, and so on one link.lock; CABOOSE_ENV would
// make every environment one.
func DaemonGetenv(getenv config.Env) config.Env {
	return func(k string) string {
		switch k {
		case "CABOOSE_DATA_DIR", "CABOOSE_ENV", "CABOOSE_SESSION":
			return ""
		}
		return getenv(k)
	}
}

// DaemonServes says whether the daemon runs the link of a command with
// this environment. The daemon serves environments at their default data
// dir only; a command run with CABOOSE_DATA_DIR set keeps its own data dir
// and so needs a link of its own (a detached `caboose link --background`),
// whose link.lock is that data dir's.
func DaemonServes(getenv config.Env) bool {
	return getenv("CABOOSE_DATA_DIR") == ""
}

// DaemonApp makes the App an environment's link runs on: its config.toml
// loaded with getenv, its sandbox on the docker found through the
// daemon's PATH. A missing environment is daemonproto's unknown_env.
func DaemonApp(getenv config.Env, checkout string) func(env string) (*App, error) {
	return func(env string) (*App, error) {
		cfg, err := config.Load(getenv, config.OSFS{}, env)
		if err != nil {
			return nil, err
		}
		if err := cfg.CheckEnv(config.OSFS{}); err != nil {
			return nil, daemonproto.NewError(daemonproto.KindUnknownEnv, "%v", err)
		}
		return &App{Cfg: cfg, Docker: &docker.CLI{Path: "docker", Stderr: io.Discard},
			Stdout: io.Discard, Stderr: io.Discard, Checkout: checkout}, nil
	}
}

// env is name's entry, made on first use; nil once the links are closed.
func (l *DaemonLinks) env(name string) *envLink {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	e := l.envs[name]
	if e == nil {
		e = &envLink{name: name, now: l.jobClock()}
		l.envs[name] = e
	}
	return e
}

func (l *DaemonLinks) logf(format string, args ...any) {
	l.mu.Lock()
	lg := l.log
	l.mu.Unlock()
	lg.Printf(format, args...)
}

func (l *DaemonLinks) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// Serve is ensure_link, restart_link or stop_link for env.
func (l *DaemonLinks) Serve(_ context.Context, method, env string) (*daemonproto.LinkResult, error) {
	if !config.ValidEnv(env) {
		return nil, daemonproto.NewError(daemonproto.KindBadRequest, "bad environment name")
	}
	e := l.env(env)
	if e == nil {
		return nil, daemonproto.NewError(daemonproto.KindBusy, "the daemon is stopping")
	}
	e.op.Lock()
	defer e.op.Unlock()
	var action string
	var err error
	switch method {
	case daemonproto.MethodEnsureLink:
		action, err = l.ensure(e, nil, false)
	case daemonproto.MethodRestartLink:
		action, err = l.ensure(e, nil, true)
	case daemonproto.MethodStopLink:
		action, err = l.stopEnv(e)
	default:
		return nil, daemonproto.NewError(daemonproto.KindUnknownMethod, "this daemon does not serve %q", method)
	}
	if err != nil {
		l.logf("%s %s: %v", method, env, err)
		var de *daemonproto.Error
		if !errors.As(err, &de) {
			err = daemonproto.NewError(daemonproto.KindInternal, "%v", err)
		}
		return nil, err
	}
	return &daemonproto.LinkResult{Action: action, Env: e.status()}, nil
}

// ensure has env's link run, with the settings its config.toml has now,
// when its sandbox runs: started when none runs, replaced when one runs
// with other settings, or, with restart, whatever it runs with. app is
// the environment's, or nil to load it. The caller holds e.op.
func (l *DaemonLinks) ensure(e *envLink, app *App, restart bool) (string, error) {
	if app == nil {
		var err error
		if app, err = l.newApp(e.name); err != nil {
			if daemonproto.Kind(err) != daemonproto.KindUnknownEnv {
				e.failed(err)
			}
			return "", err
		}
	}
	sandbox := sandboxState(app.box().State())
	e.mu.Lock()
	e.sandbox = sandbox
	cur := e.cur
	e.mu.Unlock()
	replaced := false
	if cur != nil {
		if !restart && cur.runner.settingsNow() == settingsOf(app.Cfg) {
			return daemonproto.ActionUnchanged, nil
		}
		l.stopLink(e)
		replaced = true
	}
	if sandbox != daemonproto.SandboxRunning {
		e.stopped()
		if replaced {
			return daemonproto.ActionStopped, nil
		}
		return daemonproto.ActionNoSandbox, nil
	}
	if l.isClosed() {
		return "", daemonproto.NewError(daemonproto.KindBusy, "the daemon is stopping")
	}
	if err := l.start(e, app); err != nil {
		e.failed(err)
		return "", err
	}
	if replaced {
		return daemonproto.ActionReplaced, nil
	}
	return daemonproto.ActionStarted, nil
}

// stopEnv is stop_link: the daemon's link for env stops. A link.json that
// names no daemon is a detached helper's, which this daemon neither stops
// nor marks. The caller holds e.op.
func (l *DaemonLinks) stopEnv(e *envLink) (string, error) {
	l.stopLink(e)
	e.stopped()
	app, err := l.newApp(e.name)
	if err != nil {
		if daemonproto.Kind(err) == daemonproto.KindUnknownEnv {
			return daemonproto.ActionStopped, nil
		}
		return "", err
	}
	st, ok := readLinkState(app.Cfg.DataDir)
	if !ok || st.Daemon == "" {
		return daemonproto.ActionStopped, nil
	}
	// Recovery must not bring back what was stopped on purpose. The
	// daemon it names stays.
	st.Stopped = true
	if err := writeLinkState(app.Cfg.DataDir, st); err != nil {
		return "", err
	}
	return daemonproto.ActionStopped, nil
}

// start runs a link for e on app. The caller holds e.op, and no link of
// e's runs.
func (l *DaemonLinks) start(e *envLink, app *App) error {
	dataDir := app.Cfg.DataDir
	if _, err := settingsOf(app.Cfg).check(); err != nil {
		return err
	}
	lock, unlock, err := l.takeLock(dataDir)
	if err != nil {
		return err
	}
	out, err := openCappedLog(filepath.Join(dataDir, linkLogFile), maxLinkLog)
	if err != nil {
		unlock()
		return err
	}
	in := &linkInstance{execs: hostlink.NewExecs(), done: make(chan struct{}), lock: lock}
	var decide hostlink.Decider
	if l.decide != nil {
		decide = l.decide(e.name)
	}
	var actions hostlink.Actions
	if l.actions != nil {
		actions = l.actions(e.name)
	}
	cp, cpLimits := l.agentCheckpoints(e)
	in.runner, err = app.newLinkRunner(linkRun{out: out, daemon: l.dir, execs: in.execs, gate: l.gate, decide: decide, lock: lock, actions: actions, dialogs: l.dialogs,
		checkpoints: cp, cpLimits: cpLimits})
	if err != nil {
		out.Close()
		unlock()
		return err
	}
	ctx, cancel := context.WithCancel(l.ctx)
	in.cancel = cancel
	e.mu.Lock()
	e.cur, e.state, e.since, e.lastErr = in, daemonproto.LinkRunning, time.Now(), ""
	e.mu.Unlock()
	go func() {
		defer close(in.done)
		err := in.runner.serve(ctx)
		stopped := ctx.Err() != nil
		cancel()
		out.Close()
		in.lockMu.Lock()
		in.released = true
		unlock()
		in.lockMu.Unlock()
		e.ended(in, err, stopped, app)
		// The sandbox may have stopped: its schedule looks now.
		l.schedule(e)
	}()
	l.schedule(e)
	return nil
}

// stopLink stops e's link, if one runs, and returns once it has let go of
// link.lock. The caller holds e.op.
func (l *DaemonLinks) stopLink(e *envLink) {
	e.mu.Lock()
	in := e.cur
	e.mu.Unlock()
	if in == nil {
		return
	}
	in.cancel()
	<-in.done
}

// takeLock takes dataDir's link.lock. Whoever else holds it -- another
// daemon, or a detached helper (caboose link --background) -- is left
// alone, and the lock is not taken: never a signal.
func (l *DaemonLinks) takeLock(dataDir string) (*os.File, func(), error) {
	f, unlock, err := lockLinkFile(dataDir)
	if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) {
		return f, unlock, err
	}
	if st, ok := readLinkState(dataDir); ok && st.Daemon != "" {
		return nil, nil, fmt.Errorf("the caboose daemon in %s serves this environment's link", st.Daemon)
	}
	return nil, nil, fmt.Errorf("another process holds %s (a detached 'caboose link --background', one started with CABOOSE_DATA_DIR set): the daemon does not take over its link", filepath.Join(dataDir, linkLockFile))
}

// ended records how in ended, if it is still e's link.
func (e *envLink) ended(in *linkInstance, err error, stopped bool, app *App) {
	var sandbox string
	if !stopped {
		sandbox = sandboxState(app.box().State())
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cur != in {
		return
	}
	e.cur, e.since = nil, time.Now()
	if sandbox != "" {
		e.sandbox = sandbox
	}
	var ls *linkStop
	if errors.As(err, &ls) {
		e.state, e.lastErr = daemonproto.LinkFailed, ls.msg
	} else {
		e.state = daemonproto.LinkStopped
	}
}

func (e *envLink) failed(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cur == nil {
		e.state, e.since = daemonproto.LinkFailed, time.Now()
	}
	e.lastErr = err.Error()
}

func (e *envLink) stopped() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cur == nil && e.state != daemonproto.LinkStopped {
		e.state, e.since = daemonproto.LinkStopped, time.Now()
	}
}

func (e *envLink) status() daemonproto.EnvStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := daemonproto.EnvStatus{Name: e.name, Sandbox: e.sandbox, Link: e.state, Since: e.since, LastError: clipStatus(e.lastErr)}
	if e.cur != nil {
		// A running link runs only while its sandbox does: it looks
		// before every connection, and stops when it is not.
		st.Sandbox = daemonproto.SandboxRunning
	}
	if st.Sandbox == "" {
		st.Sandbox = daemonproto.SandboxUnknown
	}
	if e.cur != nil && e.cur.runner.broker != nil {
		st.Pending = e.cur.runner.broker.Pending()
	}
	if e.cur != nil {
		if m, ok := e.cur.runner.lastMods(); ok {
			st.ModsMissing = max(0, m.Sessions-m.WithMod)
		}
	}
	if st.Link == "" {
		st.Link = daemonproto.LinkStopped
	}
	st.Job = e.job.shown(e.clock())
	return st
}

// clipStatus keeps an error, which may quote the sandbox, short and
// printable for a status line.
func clipStatus(s string) string { return daemonproto.Text(s, 300) }

// sandboxState is a Backend's State as daemonproto reports it.
func sandboxState(s string) string {
	switch s {
	case "running":
		return daemonproto.SandboxRunning
	case "absent":
		return daemonproto.SandboxAbsent
	case "exited", "created", "stopped", "dead", "paused":
		return daemonproto.SandboxStopped
	}
	return daemonproto.SandboxUnknown
}

// Envs is every environment the daemon has worked on, by name.
func (l *DaemonLinks) Envs() []daemonproto.EnvStatus {
	l.mu.Lock()
	envs := make([]*envLink, 0, len(l.envs))
	for _, e := range l.envs {
		envs = append(envs, e)
	}
	l.mu.Unlock()
	out := []daemonproto.EnvStatus{}
	for _, e := range envs {
		e.mu.Lock()
		known := e.state != "" || e.job != nil
		e.mu.Unlock()
		if known {
			st := e.status()
			st.Checkpoints = checkpointStatus(l.schedStatus(e.name), time.Now())
			l.fillStatus(e, &st)
			out = append(out, st)
		}
	}
	slices.SortFunc(out, func(a, b daemonproto.EnvStatus) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// fillStatus adds to st what the environment's config.toml and its link
// record say: the trust level, the checkpoints line status shows, and
// which other daemon serves its link. Whatever cannot be had is left out.
func (l *DaemonLinks) fillStatus(e *envLink, st *daemonproto.EnvStatus) {
	if l.newApp == nil {
		return
	}
	app, err := l.newApp(e.name)
	if err != nil {
		return
	}
	st.Trust = app.Cfg.Trust
	st.CheckpointsLine = app.checkpointLine(linkView{Env: st}, st.Sandbox == daemonproto.SandboxRunning, defaultCkSeams())
	e.mu.Lock()
	mine := e.cur != nil
	e.mu.Unlock()
	if mine {
		return
	}
	// link.json names the daemon that last started the link; a daemon of
	// another directory is the other of the release's and the checkout's.
	// It is the recorded owner, not proof that it still runs.
	if ls, ok := readLinkState(app.Cfg.DataDir); ok && !ls.Stopped && ls.Daemon != "" && filepath.Clean(ls.Daemon) != l.dir {
		st.Owner = daemonproto.OwnerRelease
		if filepath.Base(filepath.Clean(ls.Daemon)) == daemon.DevDirName {
			st.Owner = daemonproto.OwnerDev
		}
	}
}

// Busy reports whether a link runs (one runs only while its sandbox
// does), a checkpoint is being taken, or a job runs.
func (l *DaemonLinks) Busy() bool {
	if l.checkpointing() {
		return true
	}
	l.mu.Lock()
	envs := make([]*envLink, 0, len(l.envs))
	for _, e := range l.envs {
		envs = append(envs, e)
	}
	l.mu.Unlock()
	for _, e := range envs {
		e.mu.Lock()
		up := e.cur != nil || e.job.running()
		e.mu.Unlock()
		if up {
			return true
		}
	}
	return false
}

// HostExecs is how many host commands run, every link's, counting each
// running job (stop, restart, apply) as one: a replace waits for those
// too.
func (l *DaemonLinks) HostExecs() int {
	cmds, jobs := l.ExecCounts()
	return cmds + jobs
}

// ExecCounts is how many host commands run and how many jobs, read in
// one call, for a report to use both from the same read.
func (l *DaemonLinks) ExecCounts() (commands, jobs int) {
	jobs = l.runningJobs()
	commands = hostlink.HostExecsRunning()
	return commands, jobs
}

// runningJobs is how many jobs run.
func (l *DaemonLinks) runningJobs() int {
	l.mu.Lock()
	envs := make([]*envLink, 0, len(l.envs))
	for _, e := range l.envs {
		envs = append(envs, e)
	}
	l.mu.Unlock()
	n := 0
	for _, e := range envs {
		e.mu.Lock()
		if e.job.running() {
			n++
		}
		e.mu.Unlock()
	}
	return n
}

// DrainHostExecs refuses every link's host-exec requests from now on and
// reports true, or reports false while one is in flight, holding new ones
// off meanwhile if hold (daemon.Links).
func (l *DaemonLinks) DrainHostExecs(hold bool) bool { return l.gate.Drain(hold) }

// Close stops every link, each killing the host commands it runs, and
// returns once all have let go of their link.lock. No link starts after.
func (l *DaemonLinks) Close() {
	l.mu.Lock()
	l.closed = true
	envs := make([]*envLink, 0, len(l.envs))
	for _, e := range l.envs {
		envs = append(envs, e)
	}
	l.mu.Unlock()
	l.cancel()
	for _, e := range envs {
		e.op.Lock()
		l.stopLink(e)
		e.op.Unlock()
	}
	// No link's dialog is queued or timed out after.
	l.dialogs.close()
	// The schedules end at the cancel; a checkpoint being copied stops
	// (a whole-tree clone finishes first).
	l.schedWG.Wait()
}

// WillRecover says that Recover is to run: until it has, Recovering
// reports true, so that status tells a link not back yet from one this
// daemon does not serve. The daemon calls it before it serves anyone.
func (l *DaemonLinks) WillRecover() { l.recovering.Store(true) }

// Recovering reports whether Recover, announced by WillRecover, has yet
// to look at every environment.
func (l *DaemonLinks) Recovering() bool { return l.recovering.Load() }

// Recover starts the links a daemon of this directory ran before it went
// (it crashed, or was replaced): those of every environment whose
// link.json names this daemon, and whose sandbox runs, unless
// stop_link stopped it (linkState.Stopped). What it
// cannot start is logged and shown by status, never fatal. lg is the
// daemon's log, which the links' operations log to from now on.
func (l *DaemonLinks) Recover(lg *log.Logger) {
	defer l.recovering.Store(false)
	if lg != nil {
		l.mu.Lock()
		l.log = lg
		l.mu.Unlock()
	}
	if l.home == "" {
		return
	}
	entries, err := os.ReadDir(filepath.Join(l.home, "envs"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			l.logf("recovery: %v", err)
		}
		return
	}
	for _, ent := range entries {
		name := ent.Name()
		if !ent.IsDir() || !config.ValidEnv(name) {
			continue
		}
		if l.isClosed() {
			return
		}
		app, err := l.newApp(name)
		if err != nil {
			l.logf("recovery: environment %s: %v", name, err)
			continue
		}
		st, ok := readLinkState(app.Cfg.DataDir)
		if !ok || !l.claims(st) {
			continue
		}
		e := l.env(name)
		if e == nil {
			return
		}
		e.op.Lock()
		action, err := l.ensure(e, app, false)
		e.op.Unlock()
		// Its schedule runs whether or not the sandbox does: the prunes,
		// and the leftovers of a daemon that went mid-checkpoint.
		l.schedule(e)
		switch {
		case err != nil:
			l.logf("recovery: environment %s: %v", name, err)
		case action != daemonproto.ActionNoSandbox:
			l.logf("recovery: environment %s: link %s", name, action)
		}
	}
}

// claims reports whether recovery takes over the link st describes.
func (l *DaemonLinks) claims(st linkState) bool {
	return !st.Stopped && st.Daemon != "" && filepath.Clean(st.Daemon) == l.dir
}
