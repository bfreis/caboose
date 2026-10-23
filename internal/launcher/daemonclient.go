package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
)

// DaemonCaller is what the commands ask of the daemon; the client of
// internal/daemonproto is one. A seam for the tests.
type DaemonCaller interface {
	EnsureLink(ctx context.Context, env string) (*daemonproto.LinkResult, error)
	RestartLink(ctx context.Context, env string) (*daemonproto.LinkResult, error)
	Status(ctx context.Context) (*daemonproto.StatusResult, error)
	Close() error
}

// Budgets for the commands' calls: a launch waits for the daemon to start
// (it asks a login shell for its environment first) and for a link to be
// replaced, but never for a daemon that hangs.
const (
	daemonLinkBudget   = 40 * time.Second
	daemonLinkRequest  = 25 * time.Second
	daemonStatusBudget = 5 * time.Second
)

// recoveryWait bounds how long status and doctor wait for a daemon that
// has just started to bring back the links it ran, before saying it is
// still at it; restartRecoveryWait is caboose daemon restart's, which
// returns once they are back. Variables for the tests.
var (
	recoveryWait        = 3 * time.Second
	restartRecoveryWait = 15 * time.Second
)

// recoveryPoll is how often a wait for recovery asks the daemon again.
const recoveryPoll = 50 * time.Millisecond

// awaitRecovery asks for the daemon's status again, from st, while it is
// recovering and done(st) is false, for at most wait or until ctx ends.
// It returns the last status it had: on an error, st.
func awaitRecovery(ctx context.Context, c interface {
	Status(context.Context) (*daemonproto.StatusResult, error)
}, st *daemonproto.StatusResult, wait time.Duration, done func(*daemonproto.StatusResult) bool) *daemonproto.StatusResult {
	deadline := time.Now().Add(wait)
	for st.Daemon.Recovering && !done(st) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return st
		case <-time.After(recoveryPoll):
		}
		next, err := c.Status(ctx)
		if err != nil {
			return st
		}
		st = next
	}
	return st
}

// envIn is env's entry in st, or nil.
func envIn(st *daemonproto.StatusResult, env string) *daemonproto.EnvStatus {
	for i := range st.Envs {
		if st.Envs[i].Name == env {
			return &st.Envs[i]
		}
	}
	return nil
}

// runDaemonHint is what to run when the daemon does not answer.
const runDaemonHint = "caboose daemon restart"

// dialDaemon connects to the daemon: with start, starting it when nothing
// answers; without, only asking, so that nothing is started or created.
func (a *App) dialDaemon(ctx context.Context, start bool) (DaemonCaller, error) {
	if a.DaemonDial != nil {
		return a.DaemonDial(ctx, start)
	}
	if a.Cfg.CabooseHome == "" {
		return nil, errors.New("neither CABOOSE_HOME nor HOME is set: the daemon has no directory")
	}
	cfg := a.daemonConfig()
	cfg.RequestTimeout = daemonLinkRequest
	var (
		cl  *daemonproto.Client
		err error
	)
	if start {
		// An older protocol's daemon is replaced by this caboose's.
		cl, err = daemon.Ensure(ctx, cfg)
	} else {
		cl, err = daemon.Connect(ctx, cfg)
	}
	if err != nil {
		return nil, err
	}
	if cl.Compat() == daemonproto.CompatClientNewer {
		// Not replaced (asking only), or replaced by another command's
		// older caboose meanwhile: either way not one to use.
		e := olderDaemon(cl)
		cl.Close()
		return nil, e
	}
	return cl, nil
}

// servedByDaemon says whether the daemon runs this command's environment's
// link: not when CABOOSE_DATA_DIR is set, which the daemon never reads.
func (a *App) servedByDaemon() bool {
	if a.Cfg.Getenv == nil {
		return DaemonServes(os.Getenv)
	}
	return DaemonServes(a.getenv)
}

// startLink has the environment's link run for this launch, best effort: a
// launch never fails for it. The daemon runs it (and is started when nothing
// answers); one failure is said once, with the command that fixes it. A
// command run with CABOOSE_DATA_DIR set keeps its link in a detached
// helper of its own (startDetachedLink): the daemon never reads that
// variable.
func (a *App) startLink() {
	if !a.servedByDaemon() {
		a.startDetachedLink()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonLinkBudget)
	defer cancel()
	c, err := a.dialDaemon(ctx, true)
	if err != nil {
		a.Note("no link to the sandbox: the caboose daemon could not be reached: %s\n  run: %s", daemonErrText(err), runDaemonHint)
		return
	}
	defer c.Close()
	// What stopped the last link is not this one's: as a launch has always
	// forgotten it before it starts a link, so awaitProxy cannot read it.
	clearLinkStop(a.Cfg.DataDir)
	r, err := c.EnsureLink(ctx, a.Cfg.Env)
	if err != nil {
		a.Note("no link to the sandbox: the caboose daemon would not start it: %s\n  run: %s", daemonErrText(err), runDaemonHint)
		return
	}
	if r.Env.Link == daemonproto.LinkFailed {
		a.Note("no link to the sandbox: the caboose daemon could not start it: %s\n  run: %s, then caboose link", or(r.Env.LastError, "no reason given"), runDaemonHint)
	}
}

// daemonErrText is err as one line for a note.
func daemonErrText(err error) string {
	var de *daemonproto.Error
	if errors.As(err, &de) {
		return or(de.Message, de.Kind)
	}
	return firstLine(err.Error())
}

// linkViaDaemon is `caboose link` (or --restart) for an environment the
// daemon serves: it asks the daemon, starting it if need be, and says what
// came of it.
func (a *App) linkViaDaemon(restart bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), daemonLinkBudget)
	defer cancel()
	c, err := a.dialDaemon(ctx, true)
	if err != nil {
		return Die("the caboose daemon could not be reached: %s\n  run: %s", daemonErrText(err), runDaemonHint)
	}
	defer c.Close()
	var r *daemonproto.LinkResult
	clearLinkStop(a.Cfg.DataDir)
	if restart {
		r, err = c.RestartLink(ctx, a.Cfg.Env)
	} else {
		r, err = c.EnsureLink(ctx, a.Cfg.Env)
	}
	if err != nil {
		return Die("the caboose daemon would not run the link: %s\n  run: %s", daemonErrText(err), runDaemonHint)
	}
	switch r.Action {
	case daemonproto.ActionNoSandbox, daemonproto.ActionStopped:
		a.Note("%s %s is not running, so there is no link; the next launch starts one", a.noun(), a.Cfg.Container)
	case daemonproto.ActionUnchanged:
		a.Note("the link already runs with the settings in config.toml")
	case daemonproto.ActionReplaced:
		a.Note("the daemon replaced the link (log: %s)", filepath.Join(a.Cfg.DataDir, linkLogFile))
	default:
		a.Note("the daemon started the link (log: %s)", filepath.Join(a.Cfg.DataDir, linkLogFile))
	}
	if r.Env.Link == daemonproto.LinkFailed {
		return Die("the link could not run: %s", or(r.Env.LastError, "no reason given"))
	}
	return nil
}

// linkView is how the environment's link is, as status and doctor tell it.
type linkView struct {
	// Daemon is the daemon's own account, nil when it does not answer
	// (Err says why; unavailable when none runs).
	Daemon      *daemonproto.StatusResult
	Err         error
	Unavailable bool
	// Served is false when CABOOSE_DATA_DIR is set: the link, if any, is a
	// detached helper, and Up is link.lock's.
	Served bool
	// Env is the daemon's account of this environment, nil if it knows none.
	Env *daemonproto.EnvStatus
	// Up says a link runs for this environment now.
	Up bool
	// Why says why it does not, when it does not.
	Why string
}

// linkView asks the daemon, never starting it and creating nothing.
func (a *App) linkView() linkView {
	v := linkView{Served: a.servedByDaemon()}
	ctx, cancel := context.WithTimeout(context.Background(), daemonStatusBudget)
	defer cancel()
	c, err := a.dialDaemon(ctx, false)
	if err == nil {
		defer c.Close()
		v.Daemon, err = c.Status(ctx)
	}
	if err != nil {
		v.Err = err
		v.Unavailable = daemonproto.Kind(err) == daemonproto.KindUnavailable
	}
	if !v.Served {
		v.Up = detachedLinkHeld(a.Cfg.DataDir)
		if !v.Up {
			v.Why = "no helper holds link.lock"
		}
		return v
	}
	switch {
	case v.Unavailable:
		v.Why = "the daemon is not running"
		return v
	case daemonproto.Kind(v.Err) == daemonproto.KindProtocolMismatch:
		v.Why = "the daemon is another caboose's, of another protocol"
		return v
	case v.Err != nil:
		v.Why = "the daemon does not answer"
		return v
	}
	// A daemon that has just started may not have brought this
	// environment's link back yet: waited for, briefly.
	v.Daemon = awaitRecovery(ctx, c, v.Daemon, recoveryWait, func(st *daemonproto.StatusResult) bool {
		return envIn(st, a.Cfg.Env) != nil
	})
	v.Env = envIn(v.Daemon, a.Cfg.Env)
	switch {
	case v.Env == nil && v.Daemon.Daemon.Recovering:
		v.Why = "the daemon has just started, and is still bringing back the links it ran"
	case v.Env == nil:
		v.Why = "the daemon does not serve this environment"
	case v.Env.Link != daemonproto.LinkRunning:
		v.Why = "the daemon's link is " + v.Env.Link
		if v.Env.LastError != "" {
			v.Why += ": " + v.Env.LastError
		}
	case !detachedLinkHeld(a.Cfg.DataDir):
		// The daemon says it runs; its lock says otherwise.
		v.Why = "the daemon reports a link, but link.lock is free"
	default:
		v.Up = true
	}
	return v
}

// detachedLinkHeld reports whether something holds the data dir's
// link.lock, creating nothing: a missing lock is a free one.
func detachedLinkHeld(dataDir string) bool {
	if _, err := os.Stat(filepath.Join(dataDir, linkLockFile)); err != nil {
		return false
	}
	held, _ := linkRunning(dataDir)
	return held
}

// daemonLine is status's "daemon    :" value, from a linkView.
func (a *App) daemonLine(v linkView) string {
	switch {
	case v.Unavailable:
		return fmt.Sprintf("not running (%s); any caboose command that launches or links starts it, or run: caboose daemon restart", a.daemonDir())
	case daemonproto.Kind(v.Err) == daemonproto.KindProtocolMismatch:
		return daemonErrText(v.Err)
	case v.Err != nil:
		return fmt.Sprintf("does not answer (%s); run: %s", daemonErrText(v.Err), runDaemonHint)
	}
	d := v.Daemon.Daemon
	kind := ""
	if d.Dev {
		kind = ", checkout build"
	}
	s := fmt.Sprintf("running (pid %d, caboose %s%s) since %s", d.PID, d.Version, kind, d.Started.Local().Format(time.DateTime))
	if !v.Served {
		s += "; not used by this command: CABOOSE_DATA_DIR is set"
	}
	return s
}

// linkLine is status's "link      :" value.
func (a *App) linkLine(v linkView, sandboxRunning bool) string {
	hint := ""
	if sandboxRunning {
		hint = "; run: caboose link"
	}
	switch {
	case !v.Served && v.Up:
		return "running (a detached helper: CABOOSE_DATA_DIR is set)"
	case !v.Served:
		return "not running (CABOOSE_DATA_DIR is set, so a launch keeps a detached helper)" + hint
	case v.Up:
		s := "running"
		if !v.Env.Since.IsZero() {
			s += " since " + v.Env.Since.Local().Format(time.DateTime)
		}
		return s
	case v.Unavailable || v.Err != nil:
		return "not running (" + v.Why + ")" + "; run: " + runDaemonHint
	}
	s := "not running (" + v.Why
	if v.Env != nil && v.Env.Link != daemonproto.LinkRunning && !v.Env.Since.IsZero() {
		s += ", since " + v.Env.Since.Local().Format(time.DateTime)
	}
	return s + ")" + hint
}
