package launcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/selfupdate"
	"github.com/bfreis/caboose/internal/uiport"
)

// daemonDir is this caboose's daemon directory: a build from a checkout
// runs a daemon of its own, beside the release's.
func (a *App) daemonDir() string {
	return daemon.Dir(a.Cfg.CabooseHome, a.installed().kind == installDev)
}

// daemonConfig is how this caboose reaches its daemon, and starts it.
func (a *App) daemonConfig() daemon.Config {
	return daemon.Config{Dir: a.daemonDir(), Version: currentVersion(), Spawn: a.spawnDaemon, Protocol: daemonProtocol}
}

// stopConfig is daemonConfig for caboose daemon stop and restart, which
// say what a stop waits on when it is the host commands' end.
func (a *App) stopConfig() daemon.Config {
	cfg := a.daemonConfig()
	cfg.Stopping = func(n int, wait time.Duration) {
		fmt.Fprintf(a.Stdout, "caboose daemon: stopping, ending %d host %s (waiting up to %v)\n", n, plural(n, "command", "commands"), wait)
	}
	return cfg
}

// daemonProtocol is the protocol this caboose claims to its daemon; 0 is
// daemonproto's. The tests set it to play a newer caboose.
var daemonProtocol = 0

// olderDaemon is what a command that starts nothing says of a daemon of an
// older protocol: it is not used, and the next launch replaces it.
func olderDaemon(c *daemonproto.Client) *daemonproto.Error {
	d := c.Daemon()
	return daemonproto.NewError(daemonproto.KindProtocolMismatch,
		"an older caboose's daemon (caboose %s, protocol %d) runs; the next launch replaces it, or run: caboose daemon restart", d.Version, d.Protocol)
}

// spawnDaemon starts `caboose daemon` detached, with a minimal environment
// of its own: never this terminal's SSH_AUTH_SOCK, DOCKER_HOST or
// CABOOSE_ variables, which would then be the daemon's for every command
// after.
func (a *App) spawnDaemon() error {
	exe, err := a.executable()
	if err != nil {
		return err
	}
	return a.startDaemon(exe)
}

// startDaemon starts exe as `caboose daemon`, detached, as spawnDaemon says.
func (a *App) startDaemon(exe string) error {
	if a.Spawn != nil {
		return a.Spawn(exe, "daemon")
	}
	cmd, err := startDetached(exe, []string{"daemon"}, daemon.MinimalEnv(a.getenv, a.Cfg.CabooseHome))
	if err != nil {
		return err
	}
	// Not reaped: should the daemon exit while this command still runs
	// (or after it execs docker for an attach), it stays a zombie until
	// this process ends, as a.spawn's children do. Harmless.
	return cmd.Process.Release()
}

// startDetached is daemon.StartDetached; a variable for the tests.
var startDetached = daemon.StartDetached

// Daemon is `caboose daemon [status | stop | restart]`. With no argument it
// is the daemon, in the foreground, until it is stopped or idle.
func (a *App) Daemon(args []string) error {
	if a.Cfg.CabooseHome == "" {
		return Die("neither CABOOSE_HOME nor HOME is set: the daemon has no directory")
	}
	if len(args) > 1 {
		return Die("usage: caboose daemon [status | stop | restart]")
	}
	ctx := context.Background()
	if len(args) == 0 {
		return a.runDaemon()
	}
	switch args[0] {
	case "status":
		return a.daemonStatus(ctx)
	case "stop":
		running, err := daemon.Stop(ctx, a.stopConfig())
		if err != nil {
			return Die("%v", err)
		}
		if running {
			fmt.Fprintln(a.Stdout, "caboose daemon: stopped")
		} else {
			fmt.Fprintln(a.Stdout, "caboose daemon: not running")
		}
		return nil
	case "restart":
		if _, err := daemon.Stop(ctx, a.stopConfig()); err != nil {
			return Die("%v", err)
		}
		c, err := daemon.Ensure(ctx, a.daemonConfig())
		if err != nil {
			return Die("%v", err)
		}
		defer c.Close()
		st, err := c.Status(ctx)
		if err != nil {
			return Die("the daemon started, but %v", err)
		}
		// Done once the links the old daemon ran are back, so that a
		// status or doctor right after finds them.
		st = awaitRecovery(ctx, c, st, restartRecoveryWait, func(*daemonproto.StatusResult) bool { return false })
		if st.Daemon.Recovering {
			fmt.Fprintf(a.Stdout, "caboose daemon: restarted (pid %d), still bringing back the links it ran ('caboose daemon status' shows them)\n", st.Daemon.PID)
			return nil
		}
		fmt.Fprintf(a.Stdout, "caboose daemon: restarted (pid %d)\n", st.Daemon.PID)
		return nil
	}
	return Die("usage: caboose daemon [status | stop | restart] (got %q)", args[0])
}

// daemonLinksSetup, when set, runs on the daemon's links before it
// serves: a test binary playing the daemon sets what only a running link
// would give it. Nil outside the tests.
var daemonLinksSetup func(*DaemonLinks)

// daemonWeb is where the daemon serves its web page: 127.0.0.1:7171, or
// 7172 for a checkout's build. A variable for the tests, whose daemons
// serve theirs on 127.0.0.1:0 and never on either port.
var daemonWeb = func(dev bool) *daemon.WebOptions { return &daemon.WebOptions{Addr: uiport.Addr(dev)} }

// runDaemon is the daemon itself, logging to its directory's daemon.log,
// and running the environments' links (DaemonLinks). A signal stops it:
// the links stop, each ending the host commands it ran, and whatever host
// command is somehow left is killed before the process goes.
func (a *App) runDaemon() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	// Before any goroutine: exec.LookPath, ssh and docker read the process's
	// environment, so the daemon is the same whichever terminal started it.
	notes := captureLoginEnv(ctx)
	dir := a.daemonDir()
	dev := a.installed().kind == installDev
	links := NewDaemonLinks(DaemonLinksOptions{Dir: dir, Dev: dev, CabooseHome: a.Cfg.CabooseHome,
		NewApp: DaemonApp(DaemonGetenv(a.getenv), a.Checkout), Checkpoints: &CheckpointOptions{Notify: SystemNotify},
		JobNotify: func(string) func(title, text string) { return SystemNotify }})
	if daemonLinksSetup != nil {
		daemonLinksSetup(links)
	}
	// Ready recovers the links; until it has, status says so.
	links.WillRecover()
	defer hostlink.KillHostExecs()
	err := daemon.Run(ctx, daemon.Options{Dir: dir, Version: currentVersion(), Dev: dev, LockWait: daemon.StartWait,
		Links: links, Brokers: links, Checkpoints: links, Writable: links, Details: links, Proposals: links, Web: daemonWeb(dev), Ready: func(lg *log.Logger) {
			for _, n := range notes {
				lg.Print(n)
			}
			links.Recover(lg)
		},
		OnStop: func(st daemon.Stopped, lg *log.Logger) {
			if !dev {
				a.respawnDaemon(ctx, st, lg)
			}
		}})
	if errors.Is(err, daemon.ErrRunning) {
		return Die("a caboose daemon already runs for %s ('caboose daemon status' says which)", dir)
	}
	if err != nil {
		return Die("%v", err)
	}
	return nil
}

// ensureDaemon is daemon.Ensure; a variable for the tests.
var ensureDaemon = daemon.Ensure

// respawnDaemon follows a daemon's exit for a replace that was not forced
// (an update, once no host command ran): when a link ran and the version
// the replace named is installed, it starts that version's daemon, so the
// sandboxes get their links back (its recovery) without waiting for the
// next command. Under start.lock (daemon.Ensure), so that a command racing
// to start one ends with one daemon. Best effort, logged. A forced
// replace's caboose starts its own.
func (a *App) respawnDaemon(ctx context.Context, st daemon.Stopped, lg *log.Logger) {
	if !st.Replaced || !st.Busy || !selfupdate.Valid(st.Version) {
		return
	}
	exe := filepath.Join(a.layout().Versions, st.Version, selfupdate.Binary)
	if fi, err := os.Stat(exe); err != nil || !fi.Mode().IsRegular() {
		lg.Printf("replace: caboose %s is not installed (%s): the next command starts the daemon", st.Version, exe)
		return
	}
	if _, ok := a.layout().Managed(exe); !ok {
		lg.Printf("replace: %s is not an install of caboose's: the next command starts the daemon", exe)
		return
	}
	cfg := daemon.Config{Dir: a.daemonDir(), Version: currentVersion(), Spawn: func() error { return a.startDaemon(exe) }}
	c, err := ensureDaemon(ctx, cfg)
	switch {
	case err == nil:
		lg.Printf("replace: the daemon of caboose %s runs", c.Daemon().Version)
		c.Close()
	case daemonproto.Kind(err) == daemonproto.KindProtocolMismatch:
		lg.Printf("replace: caboose %s's daemon runs, of a newer protocol", st.Version)
	default:
		lg.Printf("replace: starting caboose %s's daemon: %v", st.Version, err)
	}
}

// daemonStatus asks the daemon how it is, starting nothing and creating
// nothing.
func (a *App) daemonStatus(ctx context.Context) error {
	cfg := a.daemonConfig()
	c, err := daemon.Connect(ctx, cfg)
	if daemonproto.Kind(err) == daemonproto.KindUnavailable {
		fmt.Fprintf(a.Stdout, "caboose daemon: not running (%s)\n", cfg.Dir)
		return nil
	}
	if err != nil {
		return Die("the daemon in %s does not answer: %v\n  run: caboose daemon restart", cfg.Dir, err)
	}
	defer c.Close()
	if c.Compat() == daemonproto.CompatClientNewer {
		fmt.Fprintf(a.Stdout, "caboose daemon: %s (%s)\n", olderDaemon(c).Message, cfg.Dir)
		return nil
	}
	st, err := c.Status(ctx)
	if err != nil {
		return Die("the daemon in %s: %v\n  run: caboose daemon restart", cfg.Dir, err)
	}
	d := st.Daemon
	kind := "release"
	if d.Dev {
		kind = "checkout build"
	}
	fmt.Fprintf(a.Stdout, "caboose daemon: running (pid %d, caboose %s, %s, protocol %d)\n", d.PID, d.Version, kind, d.Protocol)
	fmt.Fprintf(a.Stdout, "  since     : %s\n", d.Started.Local().Format(time.DateTime))
	fmt.Fprintf(a.Stdout, "  directory : %s\n", d.Dir)
	if cmds := max(d.HostExecs-d.Jobs, 0); cmds > 0 {
		fmt.Fprintf(a.Stdout, "  host exec : %d running\n", cmds)
	}
	if d.Jobs > 0 {
		fmt.Fprintf(a.Stdout, "  jobs      : %d running\n", d.Jobs)
	}
	if d.Replacing {
		fmt.Fprintf(a.Stdout, "  replacing : %s\n", replaceWaitText(d.HostExecs, d.Jobs))
	}
	if d.Recovering {
		fmt.Fprintf(a.Stdout, "  recovery  : bringing back the links it ran, not all listed yet\n")
	}
	for _, e := range st.Envs {
		fmt.Fprintln(a.Stdout, daemonEnvLine(e))
	}
	return nil
}

// daemonEnvLine is caboose daemon status's line for one environment.
func daemonEnvLine(e daemonproto.EnvStatus) string {
	line := fmt.Sprintf("  env %s: sandbox %s, link %s", e.Name, e.Sandbox, e.Link)
	if !e.Since.IsZero() {
		line += " since " + e.Since.Local().Format(time.DateTime)
	}
	if e.LastError != "" {
		line += " (" + e.LastError + ")"
	}
	if e.Pending > 0 {
		line += fmt.Sprintf(", %d %s pending", e.Pending, plural(e.Pending, "request", "requests"))
	}
	if j := e.Job; j != nil {
		line += fmt.Sprintf(", %s job %s", j.Kind, j.State)
		if j.Error != "" {
			line += " (" + j.Error + ")"
		}
	}
	return line
}

// replaceWaitText says what a daemon being replaced waits for: total is
// the host commands and the jobs (stop, restart, apply) together, jobs
// the jobs.
func replaceWaitText(total, jobs int) string {
	cmds := max(total-jobs, 0)
	if jobs == 0 {
		return fmt.Sprintf("waiting for %d host command(s) to end", cmds)
	}
	return fmt.Sprintf("waiting for %d host command(s) and %d job(s) to end", cmds, jobs)
}

// replaceWaitEnds is the same as a clause for a daemon that restarts
// when they have ended.
func replaceWaitEnds(total, jobs int) string {
	cmds := max(total-jobs, 0)
	if jobs == 0 {
		return fmt.Sprintf("no host command runs (%d do now)", cmds)
	}
	return fmt.Sprintf("no host command or job runs (%d and %d do now)", cmds, jobs)
}
