package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/cappedlog"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/linkdebug"
	"github.com/bfreis/caboose/internal/policy"
)

// The link helper's files, in the data dir itself: the host's, never under
// a directory the container can write.
const (
	linkLockFile = "link.lock"
	linkLogFile  = "link.log"
	// linkStateFile is the settings the running link runs with, for a
	// launch to compare its own with, and who runs it: a detached helper's
	// PID, or the directory of the daemon that serves it.
	linkStateFile = "link.json"
	// linkStopFile is why the last helper stopped for good (a linkStop),
	// for a launch waiting on the link (awaitProxy) to say it rather than
	// wait it out. A helper removes it as it starts, and so does a launch
	// before it starts one, so it is never a stale helper's.
	linkStopFile = "link.stop"
	// AgentPath is where the layer installs caboose-agent.
	AgentPath = "/usr/local/bin/caboose-agent"
)

// linkRetry bounds the wait between reconnects while the container runs.
const (
	linkRetryMin = time.Second
	linkRetryMax = 30 * time.Second
	// linkRetryEarly is the first wait after a session that ended before
	// the agent's hello: the agent is starting, not failing, so the proxy
	// should not stay down a whole linkRetryMin.
	linkRetryEarly = 200 * time.Millisecond
	// linkStopWait is how long a stop waits for the old helper to go.
	linkStopWait = 5 * time.Second
)

// linkConfigEvery is how often a link looks at config.toml; a variable
// for the tests.
var linkConfigEvery = 2 * time.Second

// linkSettings are the settings a helper runs with.
type linkSettings struct {
	ForwardPorts string `json:"forward_ports"`
	OpenURLs     string `json:"open_urls"`
	// The outbound proxy's, served only to a vm guest (serveDialed):
	// egress, and egress_ports and egress_allow as ParsePortsOf and
	// ParseAllow read them.
	Egress      bool   `json:"egress"`
	EgressPorts string `json:"egress_ports"`
	EgressAllow string `json:"egress_allow"`
	// DebugLink is linkdebug's knobs, from the environment of the launch
	// that started the helper: a launch with others starts a new one.
	DebugLink string `json:"debug_link,omitempty"`
	// Isolation is the sandbox the helper serves: a container (docker,
	// gvisor) or a VM, both called the environment's name, and the old one
	// can still be running beside the new. A helper cannot change which it
	// serves, so a launch under another isolation replaces it, and one that
	// sees config.toml change it steps aside (reload).
	Isolation string `json:"isolation"`
	// SSHAgent is ssh_agent, the agent socket a VM gets through the link,
	// and SSHAgentFrom what set it, for the log; both "" for the agent ssh
	// on this machine would use (agentFrom).
	SSHAgent     string `json:"ssh_agent,omitempty"`
	SSHAgentFrom string `json:"ssh_agent_from,omitempty"`
	// HostExec is host_exec: whether sessions run commands on this
	// machine through the link.
	HostExec bool `json:"host_exec,omitempty"`
	// Trust is trust: under "hostile" the hello offers no host exec,
	// whatever host_exec says.
	Trust string `json:"trust,omitempty"`
	// EgressDeny is egress_deny, as ParseAllow reads it: what a link with
	// no broker refuses (the broker reads it from config.toml itself).
	EgressDeny string `json:"egress_deny,omitempty"`
	// CheckpointRequests is checkpoint_requests: what a link with no
	// broker decides the checkpoints the sandbox asks for by.
	CheckpointRequests string `json:"checkpoint_requests,omitempty"`
}

func settingsOf(c *config.Config) linkSettings {
	s := linkSettings{ForwardPorts: c.ForwardPorts, OpenURLs: c.OpenURLs,
		Egress: c.Egress, EgressPorts: c.EgressPorts, EgressAllow: strings.Join(c.EgressAllow, " "),
		DebugLink: linkdebug.Parse(os.Getenv(linkdebug.Var)).Raw, Isolation: isolationOf(c), HostExec: c.HostExec,
		Trust: c.Trust, EgressDeny: strings.Join(c.EgressDeny, " "), CheckpointRequests: c.CheckpointRequests}
	if c.SSHAgent != "" {
		s.SSHAgent, s.SSHAgentFrom = c.SSHAgent, sshAgentOrigin(c)
	}
	// A Config not from config.Load leaves it unset: the default.
	if s.EgressPorts == "" {
		s.EgressPorts = config.DefaultEgressPorts
	}
	if s.CheckpointRequests == "" {
		s.CheckpointRequests = config.CheckpointRequestsAllow
	}
	return s
}

// linkConfig is the settings parsed, as the helper needs them.
type linkConfig struct {
	ports hostlink.PortSet
	// egress is nil when egress is off.
	egress *hostlink.Egress
	// hostExec is whether the hello offers host exec: host_exec, unless
	// trust is anything but fallible.
	hostExec bool
}

// check parses the settings, as the helper needs them. The outbound
// proxy's are checked whatever the isolation: a mistake in config.toml is
// said at once, not on the day it moves to vm.
func (s linkSettings) check() (linkConfig, error) {
	ports, err := hostlink.ParsePorts(s.ForwardPorts)
	if err != nil {
		return linkConfig{}, err
	}
	switch s.OpenURLs {
	case hostlink.OpenAsk, hostlink.OpenAllow, hostlink.OpenOff:
	default:
		return linkConfig{}, fmt.Errorf(`open_urls: %q is not "ask", "allow" or "off"`, s.OpenURLs)
	}
	eports, err := hostlink.ParsePortsOf("egress_ports", s.EgressPorts)
	if err != nil {
		return linkConfig{}, err
	}
	allow, err := hostlink.ParseAllow(s.EgressAllow)
	if err != nil {
		return linkConfig{}, err
	}
	deny, err := policy.ParseAllowOf("egress_deny", s.EgressDeny)
	if err != nil {
		return linkConfig{}, err
	}
	// The policy refuses host exec under any trust but fallible; the hello
	// does not even offer it then.
	lc := linkConfig{ports: ports, hostExec: s.HostExec && s.Trust == config.TrustFallible}
	if s.Egress {
		lc.egress = &hostlink.Egress{Ports: eports, Allow: allow, Deny: deny}
	}
	return lc, nil
}

// linkState is link.json.
type linkState struct {
	// PID is a detached helper's (caboose link); a link the daemon runs
	// is a goroutine, and writes none.
	PID int `json:"pid,omitempty"`
	// Daemon is the directory of the daemon that serves the link, or ""
	// for a detached helper.
	Daemon string `json:"daemon,omitempty"`
	// Stopped is set by stop_link: the link was stopped on purpose, and a
	// daemon starting again does not bring it back (DaemonLinks.Recover).
	// A link that starts writes link.json afresh, without it.
	Stopped bool `json:"stopped,omitempty"`
	linkSettings
}

func writeLinkState(dataDir string, st linkState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dataDir, linkStateFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dataDir, linkStateFile))
}

func readLinkState(dataDir string) (linkState, bool) {
	var st linkState
	b, err := os.ReadFile(filepath.Join(dataDir, linkStateFile))
	if err != nil || json.Unmarshal(b, &st) != nil {
		return st, false
	}
	return st, true
}

// writeLinkStop records why the helper stopped for good.
func writeLinkStop(dataDir, msg string) error {
	tmp := filepath.Join(dataDir, linkStopFile+".tmp")
	if err := os.WriteFile(tmp, []byte(msg), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dataDir, linkStopFile))
}

// clearLinkStop forgets the last helper's stop, before a new one runs.
func clearLinkStop(dataDir string) {
	_ = os.Remove(filepath.Join(dataDir, linkStopFile))
}

// maxLinkStop bounds what readLinkStop reads.
const maxLinkStop = 4096

// readLinkStop is why the last helper stopped for good, if it did and no
// helper has started since.
func readLinkStop(dataDir string) (string, bool) {
	f, err := os.Open(filepath.Join(dataDir, linkStopFile))
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxLinkStop))
	if err != nil || len(b) == 0 {
		return "", false
	}
	return string(b), true
}

// linkKill signals a helper; tests replace it.
var linkKill = syscall.Kill

// startDetachedLink starts `caboose link --background` for this
// environment, unless one is already running with the settings this launch
// has. One running with others -- a CABOOSE_ variable set differently, or a
// config.toml it has not reread yet -- is replaced. Best effort: a launch
// never fails for it. It is the link of a command run with CABOOSE_DATA_DIR
// set, which the daemon does not serve (startLink).
func (a *App) startDetachedLink() {
	exe, err := a.executable()
	if err != nil {
		return
	}
	if held, _ := linkRunning(a.Cfg.DataDir); held {
		if st, ok := readLinkState(a.Cfg.DataDir); ok && st.linkSettings == settingsOf(a.Cfg) {
			return
		}
		if stopLink(a.Cfg.DataDir) != nil {
			return
		}
	}
	clearLinkStop(a.Cfg.DataDir)
	_ = a.spawn(exe, "--env", a.Cfg.Env, "link", "--background")
}

// stopLink stops the running helper, if any, and waits for it to go. Its
// PID is signalled only while the lock is held, which a PID reused since
// the helper died cannot be holding: the lock is looked at again just
// before the signal, which leaves only the moment between the two. A link
// a daemon serves is never signalled: its link.json names the daemon and
// no PID, and the daemon itself would be the process.
func stopLink(dataDir string) error {
	held, err := linkRunning(dataDir)
	if err != nil || !held {
		return err
	}
	st, ok := readLinkState(dataDir)
	switch {
	case ok && st.Daemon != "":
		return fmt.Errorf("the link is the caboose daemon's in %s, not a process of its own", st.Daemon)
	case !ok || st.PID <= 1:
		return errors.New("a link is running, but its link.json does not say which process it is")
	case st.PID == os.Getpid():
		return errors.New("the running link's link.json names this process")
	}
	if held, err := linkRunning(dataDir); err != nil || !held {
		return err
	}
	if err := linkKill(st.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("cannot stop the link (pid %d): %v", st.PID, err)
	}
	for deadline := time.Now().Add(linkStopWait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if held, _ := linkRunning(dataDir); !held {
			return nil
		}
	}
	return fmt.Errorf("the link (pid %d) did not stop within %v", st.PID, linkStopWait)
}

// linkRunning reports whether a link helper holds the lock.
func linkRunning(dataDir string) (bool, error) {
	unlock, err := lockLink(dataDir)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
	unlock()
	return false, nil
}

func lockLink(dataDir string) (func(), error) {
	_, unlock, err := lockLinkFile(dataDir)
	return unlock, err
}

// lockLinkFile takes dataDir's link.lock, and returns the file that holds
// it, open until unlock: the broker tells whether this process still owns
// the environment by it (broker.LockOwner).
func lockLinkFile(dataDir string) (*os.File, func(), error) {
	f, err := os.OpenFile(filepath.Join(dataDir, linkLockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Link is `caboose link`. The daemon runs the environment's link, so it
// asks the daemon to (starting it when nothing answers); --restart has the
// daemon replace the link whatever its settings. --background is the
// detached helper's mode, the link of a command run with CABOOSE_DATA_DIR
// set, which the daemon does not serve: it logs to the data dir's link.log
// instead of the terminal, and exits quietly when another one already
// runs. --foreground runs the link in this process, for debugging, and is
// refused while the daemon serves the environment. With CABOOSE_DATA_DIR
// set, plain `caboose link` runs it in the foreground and --restart
// replaces the helper, as they always did.
//
// A link, wherever it runs, rereads config.toml itself when it changes,
// tells of new proposals (watchProposals), checks those for packages
// (checkProposals), and, under vm, warns when the VM holds many of the
// Mac's vnodes (watchVnodes).
func (a *App) Link(args []string) error {
	const usage = "usage: caboose link [--restart | --foreground | --background]"
	background, restart, foreground := false, false, false
	for _, arg := range args {
		switch arg {
		case "--background":
			background = true
		case "--restart":
			restart = true
		case "--foreground":
			foreground = true
		default:
			return Die("%s (got %q)", usage, arg)
		}
	}
	n := 0
	for _, on := range []bool{background, restart, foreground} {
		if on {
			n++
		}
	}
	if n > 1 {
		return Die("%s", usage)
	}
	if _, err := settingsOf(a.Cfg).check(); err != nil {
		return Die("%v", err)
	}
	if foreground {
		if msg := a.daemonServingLink(); msg != "" {
			return Die("%s", msg)
		}
	}
	if !background && !foreground && a.servedByDaemon() {
		return a.linkViaDaemon(restart)
	}
	if restart {
		return a.restartLink()
	}
	lock, unlock, err := lockLinkFile(a.Cfg.DataDir)
	if err != nil {
		if background {
			return nil
		}
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return Die("a link is already running for this environment (see %s; 'caboose link --restart' replaces it)", filepath.Join(a.Cfg.DataDir, linkLogFile))
		}
		return Die("cannot lock %s: %v", filepath.Join(a.Cfg.DataDir, linkLockFile), err)
	}
	defer unlock()

	var out io.Writer = a.Stderr
	if background {
		c, err := openCappedLog(filepath.Join(a.Cfg.DataDir, linkLogFile), maxLinkLog)
		if err != nil {
			return nil
		}
		defer c.Close()
		out = c
	}
	stop := make(chan struct{})
	defer close(stop)
	go killHostExecsAtSignal(stop)
	r, err := a.newLinkRunner(linkRun{out: out, lock: lock})
	if err != nil {
		return Die("%v", err)
	}
	err = r.serve(context.Background())
	var ls *linkStop
	if errors.As(err, &ls) && !background {
		return Die("%s", ls.msg)
	}
	return nil
}

// linkRun is how a link runs: as a helper of its own (caboose link), or
// as one of the daemon's links.
type linkRun struct {
	// out is where it logs.
	out io.Writer
	// daemon is the directory of the daemon that serves it, "" for a
	// helper of its own: link.json says which.
	daemon string
	// execs, when set, tracks the link's host commands apart from any
	// other link's in the process.
	execs *hostlink.Execs
	// gate, when set, admits the link's host-exec requests (the daemon's,
	// shared by its links).
	gate *hostlink.ExecGate
	// decide makes the link's decisions; nil has the link open the
	// environment's broker (lock set) or decide as config.toml says.
	decide hostlink.Decider
	// lock is the link.lock the caller holds: the broker serves only
	// while it is held.
	lock *os.File
	// actions is this machine's browser, notifications and dialogs; nil
	// is hostlink.System's.
	actions hostlink.Actions
	// dialogs is the URL dialog queue the link shares with the process's
	// other links (the daemon's); nil is one of its own.
	dialogs *dialogQueue
	// checkpoints takes the checkpoints the sandbox asks for, within
	// cpLimits, the environment's; nil (a helper of its own) offers none.
	checkpoints hostlink.Checkpointer
	cpLimits    *hostlink.CheckpointLimits
}

// newLinkRunner is a link for this environment, as config.toml has it
// now, not yet running. The caller holds link.lock.
func (a *App) newLinkRunner(o linkRun) (*linkRunner, error) {
	settings := settingsOf(a.Cfg)
	lc, err := settings.check()
	if err != nil {
		return nil, err
	}
	r := &linkRunner{a: a, log: log.New(o.out, "caboose link: ", log.LstdFlags), settings: settings,
		daemon: o.daemon, execs: o.execs, dialogs: o.dialogs,
		cfg: hostlink.Config{Ports: lc.ports, OpenURL: settings.OpenURLs, Actions: hostlink.System{}, Egress: lc.egress, Trust: settings.Trust,
			HostExec: hostExecOffer(lc.hostExec), Diagnose: true, Decide: o.decide, Execs: o.execs, Gate: o.gate}}
	r.cfg.Log = r.log
	r.cfg.Mods = r.setMods
	r.cfg.Forwarded = r.setForwards
	r.cfg.AliasTmp = aliasTmp(a.daemonDir())
	r.cfg.Checkpoints, r.cfg.CheckpointLimits = o.checkpoints, o.cpLimits
	r.cfg.CheckpointRequests = settings.CheckpointRequests
	if o.actions != nil {
		r.cfg.Actions = o.actions
	}
	if o.decide == nil && o.lock != nil {
		b, err := r.openBroker(o.lock)
		if err != nil {
			return nil, fmt.Errorf("cannot open the environment's request broker: %v", err)
		}
		r.broker = b
		r.cfg.Decide = b.Link()
	}
	return r, nil
}

// serve runs the link until the sandbox stops, the link stops for good
// (a *linkStop, also written to link.stop), or ctx ends; its watchers run
// beside it. It returns once every goroutine it started has ended and
// every host command it ran is gone, so the caller may then let go of
// link.lock.
func (r *linkRunner) serve(ctx context.Context) error {
	clearLinkStop(r.a.Cfg.DataDir)
	r.writeState()
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	watchers := []func(context.Context){r.watchConfig, r.watchProposals, r.checkProposals, r.watchVnodes}
	if r.broker != nil {
		defer r.closeBroker()
		watchers = append(watchers, r.broker.Run)
	}
	for _, w := range watchers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w(ctx)
		}()
	}
	err := r.run(ctx)
	cancel()
	wg.Wait()
	if r.execs != nil {
		r.execs.Kill()
	}
	return err
}

// killHostExecsAtSignal has the commands run on this machine for the
// sandbox go with the link, until stop: a session's end kills its own,
// and so must the link's, which a signal would otherwise end with no word
// to them. The signal then ends the link as it would have.
func killHostExecsAtSignal(stop <-chan struct{}) {
	sigs := make(chan os.Signal, 1)
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		// One the link was started ignoring stays ignored.
		if !signal.Ignored(sig) {
			signal.Notify(sigs, sig)
		}
	}
	defer signal.Stop(sigs)
	select {
	case <-stop:
	case sig := <-sigs:
		hostlink.KillHostExecs()
		signal.Reset(sig)
		_ = syscall.Kill(os.Getpid(), sig.(syscall.Signal))
	}
}

// restartLink is `caboose link --restart`.
func (a *App) restartLink() error {
	held, _ := linkRunning(a.Cfg.DataDir)
	if held {
		if err := stopLink(a.Cfg.DataDir); err != nil {
			return Die("%v", err)
		}
		a.Note("stopped the running link")
	}
	exe, err := a.executable()
	if err != nil {
		return Die("cannot find this caboose to start the link: %v", err)
	}
	clearLinkStop(a.Cfg.DataDir)
	if err := a.spawn(exe, "--env", a.Cfg.Env, "link", "--background"); err != nil {
		return Die("cannot start the link: %v", err)
	}
	if state := a.box().State(); state != "running" {
		a.Note("started a link, but %s %s is %s: it stops at once, and the next launch starts one", a.noun(), a.Cfg.Container, state)
		return nil
	}
	a.Note("started a new link in the background (log: %s)", filepath.Join(a.Cfg.DataDir, linkLogFile))
	return nil
}

// linkRunner is a running link: its settings, which a change to
// config.toml replaces, and the session it serves now.
type linkRunner struct {
	a   *App
	log *log.Logger
	// daemon and execs are linkRun's.
	daemon string
	execs  *hostlink.Execs

	// broker is the environment's request broker, which decides what the
	// sandbox asks; nil when the link was given a Decider instead. dialog
	// is its URL dialog.
	broker *broker.Broker
	dialog *urlDialog
	// dialogs is the process's URL dialog queue, shared by the daemon's
	// links; nil has the link make one of its own.
	dialogs *dialogQueue

	mu       sync.Mutex
	settings linkSettings
	cfg      hostlink.Config
	sess     *agentproto.Session
	mods     *agentproto.ModsInfo // the agent's last count
	forwards []int                // the ports forwarded now, sorted
	reloaded bool                 // the session ended for new settings: reconnect at once
	retired  bool                 // the isolation changed: this helper serves no more
}

func (r *linkRunner) settingsNow() linkSettings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings
}

func (r *linkRunner) writeState() {
	r.mu.Lock()
	st := linkState{Daemon: r.daemon, linkSettings: r.settings}
	if r.daemon == "" {
		st.PID = os.Getpid()
	}
	r.mu.Unlock()
	if err := writeLinkState(r.a.Cfg.DataDir, st); err != nil {
		r.log.Printf("cannot write %s: %v", linkStateFile, err)
	}
}

// run links to the sandbox again and again, while it runs, until ctx
// ends. It returns nil, or the *linkStop that ended it for good.
func (r *linkRunner) run(ctx context.Context) error {
	a := r.a
	wait, early := linkRetryMin, false
	for {
		if ctx.Err() != nil {
			return nil
		}
		r.mu.Lock()
		retired := r.retired
		r.mu.Unlock()
		if retired {
			return nil
		}
		if state := a.box().State(); state != "running" {
			r.log.Printf("%s %s is %s: done", a.noun(), a.Cfg.Container, state)
			return nil
		}
		started := time.Now()
		err := r.once(ctx)
		if ctx.Err() != nil {
			r.log.Print("stopped")
			return nil
		}
		var stop *linkStop
		if errors.As(err, &stop) {
			r.log.Print(stop.msg)
			if err := writeLinkStop(a.Cfg.DataDir, stop.msg); err != nil {
				r.log.Printf("cannot write %s: %v", linkStopFile, err)
			}
			return stop
		}
		r.mu.Lock()
		reloaded, retired := r.reloaded, r.retired
		r.reloaded = false
		r.mu.Unlock()
		if retired {
			return nil
		}
		if reloaded {
			wait, early = linkRetryMin, false
			continue
		}
		r.log.Printf("link ended: %v", err)
		if time.Since(started) > linkRetryMax {
			wait, early = linkRetryMin, false
		}
		var pause time.Duration
		pause, wait, early = linkPause(err, wait, early)
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			r.log.Print("stopped")
			return nil
		case <-t.C:
		}
	}
}

// linkPause is the wait before the next connection after err, and the
// backoff and whether the early retry was spent, as they are after it:
// the backoff doubles from linkRetryMin to linkRetryMax, but the first
// session in a row to end before the agent's hello is retried after
// linkRetryEarly, without doubling.
func linkPause(err error, wait time.Duration, early bool) (pause, next time.Duration, spent bool) {
	if !early && wait == linkRetryMin && errors.Is(err, hostlink.ErrNoHello) {
		return linkRetryEarly, wait, true
	}
	return wait, min(2*wait, linkRetryMax), early
}

// configFingerprint is what tells config.toml changed: whether it exists,
// its size and its modification time.
func configFingerprint(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "absent"
	}
	return fmt.Sprintf("%d %d", fi.Size(), fi.ModTime().UnixNano())
}

// watchConfig rereads config.toml whenever it changes, until ctx ends.
func (r *linkRunner) watchConfig(ctx context.Context) {
	if r.a.Cfg.EnvDir == "" {
		return
	}
	path := filepath.Join(r.a.Cfg.EnvDir, config.FileName)
	last := configFingerprint(path)
	t := time.NewTicker(linkConfigEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if fp := configFingerprint(path); fp != last {
			last = fp
			r.reload()
		}
	}
}

// reload reads the settings again, as a launch would, and, when they
// changed, ends the session so the next one runs with them. Settings that
// do not parse leave the running ones in place.
func (r *linkRunner) reload() {
	if r.broker != nil {
		// The policy is read again whatever the link's own settings do:
		// trust, aliases, open_urls_allow and egress_deny are the broker's.
		// A file that does not read keeps the policy in use (Reload logs it).
		_ = r.broker.Reload()
	}
	c, err := config.Load(r.a.Cfg.Getenv, config.OSFS{}, r.a.Cfg.Env)
	if err != nil {
		r.log.Printf("config.toml changed, but cannot be read (%v): keeping the settings in use", err)
		return
	}
	next := settingsOf(c)
	lc, err := next.check()
	if err != nil {
		r.log.Printf("config.toml changed, but %v: keeping the settings in use", err)
		return
	}
	r.mu.Lock()
	if next == r.settings {
		r.mu.Unlock()
		return
	}
	if next.Isolation != r.settings.Isolation {
		r.retired = true
		sess := r.sess
		r.mu.Unlock()
		r.log.Printf("config.toml changed the isolation from %s to %s: this link serves the %s sandbox only, and stops; the next launch starts one for the %s one",
			r.settings.Isolation, next.Isolation, r.settings.Isolation, next.Isolation)
		if sess != nil {
			sess.Close()
		}
		return
	}
	r.settings = next
	r.cfg.Ports, r.cfg.OpenURL, r.cfg.Egress = lc.ports, next.OpenURLs, lc.egress
	r.cfg.HostExec = hostExecOffer(lc.hostExec)
	r.cfg.Trust = next.Trust
	r.cfg.CheckpointRequests = next.CheckpointRequests
	sess := r.sess
	r.reloaded = sess != nil
	r.mu.Unlock()
	r.writeState()
	r.log.Printf("config.toml changed: forward_ports %q, open_urls %q, egress %s, egress_ports %q, egress_allow %q, egress_deny %q, host_exec %s (trust %q); reconnecting",
		next.ForwardPorts, next.OpenURLs, onOff(next.Egress), next.EgressPorts, next.EgressAllow, next.EgressDeny, onOff(lc.hostExec), next.Trust)
	if sess != nil {
		sess.Close()
	}
}

// linkStop is an ending that retrying cannot fix. Its message says what
// does: a launch shows it as it is (awaitProxy).
type linkStop struct{ msg string }

func (e *linkStop) Error() string { return e.msg }

// closeAtCancel closes sess when ctx ends, and kill, when not nil, runs
// linkStopWait later should the session's other end still not be gone
// (gone closed). The returned function undoes it, and returns once
// nothing of it is left running.
func closeAtCancel(ctx context.Context, sess *agentproto.Session, gone <-chan struct{}, kill func()) func() {
	ran := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(ran)
		sess.Close()
		if kill == nil {
			return
		}
		t := time.NewTimer(linkStopWait)
		defer t.Stop()
		select {
		case <-t.C:
			kill()
		case <-gone:
		}
	})
	return func() {
		if !stop() {
			<-ran
		}
	}
}

// once runs one `docker exec -i ... caboose-agent link` and serves it, or
// connects to a VM's link port, until the session ends or ctx does.
func (r *linkRunner) once(ctx context.Context) error {
	a := r.a
	if l, ok := a.box().(backend.Linker); ok {
		return r.serveDialed(ctx, l)
	}
	cmd := a.box().Command(backend.ExecSpec{Argv: []string{AgentPath, "link"}, Stdin: true})
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	if err := cmd.Start(); err != nil {
		return err
	}
	sess := agentproto.NewSession(stdout, stdin, true)
	// A stop closes the agent's stdin, which ends it; one that does not
	// end is killed (its exec, here) so the stop is never held up.
	exited := make(chan struct{})
	undo := closeAtCancel(ctx, sess, exited, func() { _ = cmd.Process.Kill() })
	r.mu.Lock()
	r.sess = sess
	cfg := r.cfg
	r.mu.Unlock()
	cfg.Relay = r.relayRoots()
	cfg.HostExec = r.hostExec(cfg.HostExec)
	// The engine dials a container's connections from the host already:
	// the outbound proxy is a vm guest's alone.
	cfg.Egress = nil
	r.log.Printf("linked to %s", a.Cfg.Container)
	runErr := hostlink.Run(sess, cfg)
	r.mu.Lock()
	r.sess = nil
	r.mu.Unlock()
	sess.Close()
	waitErr := cmd.Wait()
	close(exited)
	undo()
	said := strings.TrimSpace(stderr.String())
	switch {
	case errors.Is(runErr, hostlink.ErrVersion):
		return &linkStop{runErr.Error()}
	case docker.ExitCode(waitErr) == 126 || docker.ExitCode(waitErr) == 127:
		// The exec itself failed: no agent in this image.
		return &linkStop{fmt.Sprintf("the container has no caboose-agent: 'caboose restart' recreates it from this caboose's image (%s)", said)}
	}
	if said != "" {
		return fmt.Errorf("%v (the agent said: %s)", runErr, said)
	}
	return runErr
}

// serveDialed serves the link on a connection to the sandbox's link port,
// where the agent is already running: there is no exec to fail.
func (r *linkRunner) serveDialed(ctx context.Context, l backend.Linker) error {
	conn, err := l.DialLink()
	if err != nil {
		return err
	}
	sess := agentproto.NewSession(conn, conn, true)
	// Closing the session closes conn, which ends its reads too.
	defer closeAtCancel(ctx, sess, nil, nil)()
	r.mu.Lock()
	r.sess = sess
	cfg := r.cfg
	r.mu.Unlock()
	cfg.Relay = r.relayRoots()
	cfg.HostExec = r.hostExec(cfg.HostExec)
	// No socket of this machine's can reach a VM: the link carries its SSH
	// agent, ssh_agent's or else (never under hostile) the one ssh here would use, as this
	// process sees it (its SSH_AUTH_SOCK, ~/.ssh/config): the daemon's, from
	// a login shell, or a detached helper's, from the command that started it.
	st := r.settingsNow()
	sock, from := r.a.agentFrom(st.SSHAgent, st.SSHAgentFrom, st.Trust)
	cfg.SSHAgent, cfg.SSHAuthSock = containerAgent, sock
	if sock == "" {
		r.log.Printf("no SSH agent (%s): the sandbox gets none", from)
	} else {
		r.log.Printf("SSH agent %s (%s)", sock, from)
	}
	// vmnet's NAT reaches none of this machine's VPN routes: the guest's
	// outbound connections are dialled here, unless egress is off
	// (cfg.Egress nil).
	if cfg.Egress != nil {
		r.log.Printf("serving the outbound proxy (egress_ports %q)", r.settingsNow().EgressPorts)
	}
	r.log.Printf("linked to %s", r.a.Cfg.Container)
	runErr := hostlink.Run(sess, cfg)
	r.mu.Lock()
	r.sess = nil
	r.mu.Unlock()
	sess.Close()
	if errors.Is(runErr, hostlink.ErrVersion) {
		return &linkStop{runErr.Error()}
	}
	return runErr
}

// relayRoots are the directories whose changes the link relays into the
// container: the writable parts of the roots it has mounted (a whole root
// where all of it is), when it was created under an isolation that turns
// none of the host's edits into inotify events inside (gVisor). Under runc
// the engine passes them on itself. The relay raises an event by setting
// a file's mode, which a read-only mount refuses, so the read-only rest of
// a root is not relayed.
func (r *linkRunner) relayRoots() []hostlink.Root {
	// A VM's virtio-fs raises no event for the Mac's edits either.
	iso, _, ok := r.a.createdIsolation()
	if !ok || iso != isolationGVisor && iso != isolationVM {
		return nil
	}
	return writableRelay(r.a.mountedRoots())
}

// writableRelay is relayRoots for mounted roots.
func writableRelay(mounted []config.Root) []hostlink.Root {
	var roots []hostlink.Root
	for _, m := range mounted {
		hosts, ctrs := m.WritableHost(), m.WritableContainer()
		for i := range hosts {
			roots = append(roots, hostlink.Root{Host: hosts[i], Container: ctrs[i]})
		}
	}
	return roots
}

// hostExecOffer is host_exec as the runner keeps it: an offer whose roots
// each session fills in (hostExec), nil when it is off.
func hostExecOffer(on bool) *hostlink.HostExec {
	if !on {
		return nil
	}
	return &hostlink.HostExec{}
}

// hostExec is offer for one session: the roots the sandbox has mounted,
// which a command's directory is translated through, and not the
// configured ones, which differ from them until a restart.
func (r *linkRunner) hostExec(offer *hostlink.HostExec) *hostlink.HostExec {
	if offer == nil {
		return nil
	}
	roots := r.a.mountedRoots()
	r.log.Printf("running commands on this machine for the sandbox (host_exec), in %s", mountList(roots))
	return &hostlink.HostExec{Roots: roots}
}

// limitedWriter keeps the first n bytes written to it.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// maxLinkLog is the most link.log grows to before it is moved aside, to
// link.log.1: the sandbox decides how often some of its lines are written.
const maxLinkLog = 10 << 20

// openCappedLog opens link.log, appended to by every link so a restart
// keeps what the last one said, and moved aside to link.log.1, saying so,
// rather than grow past max.
func openCappedLog(path string, max int64) (*cappedlog.Log, error) {
	return cappedlog.Open(path, max, "caboose link")
}

// daemonServingLink says, as a refusal, when the caboose daemon serves this
// environment's link; "" when it does not (or cannot be asked).
func (a *App) daemonServingLink() string {
	if st, ok := readLinkState(a.Cfg.DataDir); ok && st.Daemon != "" && detachedLinkHeld(a.Cfg.DataDir) {
		return fmt.Sprintf("the caboose daemon (%s) serves this environment's link, and one runs at a time: run 'caboose daemon stop' first, or 'caboose link --restart' to have the daemon replace it", st.Daemon)
	}
	v := a.linkView()
	if v.Served && v.Env != nil && (v.Env.Link == daemonproto.LinkRunning || v.Env.Link == daemonproto.LinkStarting) {
		return "the caboose daemon serves this environment's link, and one runs at a time: run 'caboose daemon stop' first, or 'caboose link --restart' to have the daemon replace it"
	}
	return ""
}
