package launcher

import (
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
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/linkdebug"
)

// The link helper's files, in the data dir itself: the host's, never under
// a directory the container can write.
const (
	linkLockFile = "link.lock"
	linkLogFile  = "link.log"
	// linkStateFile is the running helper's PID and the settings it runs
	// with, for a launch to compare its own with.
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
	// the agent's hello, as a VM's agent from before it held the
	// connection until its boot had started did: it is starting, not
	// failing, so the proxy should not stay down a whole linkRetryMin.
	linkRetryEarly = 200 * time.Millisecond
	// linkConfigEvery is how often the helper looks at config.toml.
	linkConfigEvery = 2 * time.Second
	// linkStopWait is how long a stop waits for the old helper to go.
	linkStopWait = 5 * time.Second
)

// linkSettings are the settings a helper runs with.
type linkSettings struct {
	ForwardPorts string `json:"forward_ports"`
	OpenURLs     string `json:"open_urls"`
	// The outbound proxy's, served only to a vm guest (serveDialed).
	EgressProxy string `json:"egress_proxy"`
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
	// HostExec is host_exec, as written: whether sessions run commands
	// on this machine through the link.
	HostExec string `json:"host_exec,omitempty"`
}

func settingsOf(c *config.Config) linkSettings {
	s := linkSettings{ForwardPorts: c.ForwardPorts, OpenURLs: c.OpenURLs,
		EgressProxy: c.EgressProxy, EgressPorts: c.EgressPorts, EgressAllow: c.EgressAllow,
		DebugLink: linkdebug.Parse(os.Getenv(linkdebug.Var)).Raw, Isolation: isolationOf(c), HostExec: c.HostExec}
	if c.SSHAgent != "" {
		s.SSHAgent, s.SSHAgentFrom = c.SSHAgent, sshAgentOrigin(c)
	}
	// A Config not from config.Load leaves them unset: the defaults.
	if s.EgressProxy == "" {
		s.EgressProxy = "on"
	}
	if s.EgressPorts == "" {
		s.EgressPorts = config.DefaultEgressPorts
	}
	return s
}

// linkConfig is the settings parsed, as the helper needs them.
type linkConfig struct {
	ports hostlink.PortSet
	// egress is nil when egress_proxy is off.
	egress *hostlink.Egress
	// hostExec is host_exec.
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
	on, err := config.CheckEgressProxy(s.EgressProxy)
	if err != nil {
		return linkConfig{}, err
	}
	eports, err := hostlink.ParsePortsOf("egress_ports", s.EgressPorts)
	if err != nil {
		return linkConfig{}, err
	}
	allow, err := hostlink.ParseAllow(s.EgressAllow)
	if err != nil {
		return linkConfig{}, err
	}
	hostExec, err := config.CheckHostExec(s.HostExec)
	if err != nil {
		return linkConfig{}, err
	}
	lc := linkConfig{ports: ports, hostExec: hostExec}
	if on {
		lc.egress = &hostlink.Egress{Ports: eports, Allow: allow}
	}
	return lc, nil
}

// linkState is link.json.
type linkState struct {
	PID int `json:"pid"`
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

// startLink starts `caboose link --background` for this environment,
// unless one is already running with the settings this launch has. One
// running with others -- a CABOOSE_ variable set differently, or a
// config.toml it has not reread yet -- is replaced. Best effort: a launch
// never fails for it.
func (a *App) startLink() {
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
// the helper died cannot be holding.
func stopLink(dataDir string) error {
	held, err := linkRunning(dataDir)
	if err != nil || !held {
		return err
	}
	st, ok := readLinkState(dataDir)
	if !ok || st.PID <= 1 {
		return errors.New("a link is running, but its link.json does not say which process it is")
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
	f, err := os.OpenFile(filepath.Join(dataDir, linkLockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Link is `caboose link`: the host's end of the link to caboose-agent in the
// container, for as long as the container runs. With --background (what a
// launch starts) it logs to the data dir's link.log instead of the
// terminal, and exits quietly when another one already runs. --restart
// stops the running one and starts another in the background, for
// settings a launch would not notice changed. The running one rereads
// config.toml itself when it changes, and tells of new proposals
// (watchProposals).
func (a *App) Link(args []string) error {
	background, restart := false, false
	for _, arg := range args {
		switch arg {
		case "--background":
			background = true
		case "--restart":
			restart = true
		default:
			return Die("usage: caboose link [--background | --restart] (got %q)", arg)
		}
	}
	if background && restart {
		return Die("usage: caboose link [--background | --restart]")
	}
	settings := settingsOf(a.Cfg)
	lc, err := settings.check()
	if err != nil {
		return Die("%v", err)
	}
	if restart {
		return a.restartLink()
	}
	unlock, err := lockLink(a.Cfg.DataDir)
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
	clearLinkStop(a.Cfg.DataDir)

	var out io.Writer = a.Stderr
	if background {
		c, err := openCappedLog(filepath.Join(a.Cfg.DataDir, linkLogFile), maxLinkLog)
		if err != nil {
			return nil
		}
		defer c.Close()
		out = c
	}
	r := &linkRunner{a: a, log: log.New(out, "caboose link: ", log.LstdFlags), settings: settings,
		cfg: hostlink.Config{Ports: lc.ports, OpenURL: settings.OpenURLs, Actions: hostlink.System{}, Egress: lc.egress,
			HostExec: hostExecOffer(lc.hostExec), Diagnose: true}}
	r.cfg.Log = r.log
	r.writeState()
	stop := make(chan struct{})
	defer close(stop)
	go killHostExecsAtSignal(stop)
	go r.watchConfig(stop)
	go r.watchProposals(stop)
	return r.run(background)
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

// linkRunner is a running helper: its settings, which a change to
// config.toml replaces, and the session it serves now.
type linkRunner struct {
	a   *App
	log *log.Logger

	mu       sync.Mutex
	settings linkSettings
	cfg      hostlink.Config
	sess     *agentproto.Session
	reloaded bool // the session ended for new settings: reconnect at once
	retired  bool // the isolation changed: this helper serves no more
}

func (r *linkRunner) settingsNow() linkSettings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings
}

func (r *linkRunner) writeState() {
	r.mu.Lock()
	st := linkState{PID: os.Getpid(), linkSettings: r.settings}
	r.mu.Unlock()
	if err := writeLinkState(r.a.Cfg.DataDir, st); err != nil {
		r.log.Printf("cannot write %s: %v", linkStateFile, err)
	}
}

func (r *linkRunner) run(background bool) error {
	a := r.a
	wait, early := linkRetryMin, false
	for {
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
		err := r.once()
		var stop *linkStop
		if errors.As(err, &stop) {
			r.log.Print(stop.msg)
			if err := writeLinkStop(a.Cfg.DataDir, stop.msg); err != nil {
				r.log.Printf("cannot write %s: %v", linkStopFile, err)
			}
			if background {
				return nil
			}
			return Die("%s", stop.msg)
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
		time.Sleep(pause)
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

// watchConfig rereads config.toml whenever it changes, until stop.
func (r *linkRunner) watchConfig(stop <-chan struct{}) {
	if r.a.Cfg.EnvDir == "" {
		return
	}
	path := filepath.Join(r.a.Cfg.EnvDir, config.FileName)
	last := configFingerprint(path)
	t := time.NewTicker(linkConfigEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		if fp := configFingerprint(path); fp != last {
			last = fp
			r.reload()
		}
	}
}

// reload reads the settings again, as a launch would -- a CABOOSE_ variable
// this helper was started with still wins over the file -- and, when they
// changed, ends the session so the next one runs with them. Settings that
// do not parse leave the running ones in place.
func (r *linkRunner) reload() {
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
	sess := r.sess
	r.reloaded = sess != nil
	r.mu.Unlock()
	r.writeState()
	r.log.Printf("config.toml changed: forward_ports %q, open_urls %q, egress_proxy %q, egress_ports %q, egress_allow %q, host_exec %s; reconnecting",
		next.ForwardPorts, next.OpenURLs, next.EgressProxy, next.EgressPorts, next.EgressAllow, onOff(lc.hostExec))
	if sess != nil {
		sess.Close()
	}
}

// linkStop is an ending that retrying cannot fix. Its message says what
// does: a launch shows it as it is (awaitProxy).
type linkStop struct{ msg string }

func (e *linkStop) Error() string { return e.msg }

// once runs one `docker exec -i ... caboose-agent link` and serves it, or
// connects to a VM's link port.
func (r *linkRunner) once() error {
	a := r.a
	if l, ok := a.box().(backend.Linker); ok {
		return r.serveDialed(l)
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
	said := strings.TrimSpace(stderr.String())
	switch {
	case errors.Is(runErr, hostlink.ErrVersion):
		return &linkStop{runErr.Error()}
	case docker.ExitCode(waitErr) == 126 || docker.ExitCode(waitErr) == 127:
		// The exec itself failed: no agent in this image.
		return &linkStop{fmt.Sprintf("the container has no caboose-agent (it was created by an older caboose): 'caboose restart' moves it onto a new image (%s)", said)}
	}
	if said != "" {
		return fmt.Errorf("%v (the agent said: %s)", runErr, said)
	}
	return runErr
}

// serveDialed serves the link on a connection to the sandbox's link port,
// where the agent is already running: there is no exec to fail.
func (r *linkRunner) serveDialed(l backend.Linker) error {
	conn, err := l.DialLink()
	if err != nil {
		return err
	}
	sess := agentproto.NewSession(conn, conn, true)
	r.mu.Lock()
	r.sess = sess
	cfg := r.cfg
	r.mu.Unlock()
	cfg.Relay = r.relayRoots()
	cfg.HostExec = r.hostExec(cfg.HostExec)
	// No socket of this machine's can reach a VM: the link carries its SSH
	// agent, ssh_agent's or else the one ssh here would use, as the launch
	// that started this link saw it (its SSH_AUTH_SOCK, its ~/.ssh/config).
	st := r.settingsNow()
	sock, from := r.a.agentFrom(st.SSHAgent, st.SSHAgentFrom)
	cfg.SSHAgent, cfg.SSHAuthSock = containerAgent, sock
	if sock == "" {
		r.log.Printf("no SSH agent (%s): the sandbox gets none", from)
	} else {
		r.log.Printf("SSH agent %s (%s)", sock, from)
	}
	// vmnet's NAT reaches none of this machine's VPN routes: the guest's
	// outbound connections are dialled here, unless egress_proxy is off
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

// relayRoots are the roots whose changes the link relays into the
// container: those it has mounted, when it was created under an isolation
// that turns none of the host's edits into inotify events inside (gVisor).
// Under runc the engine passes them on itself.
func (r *linkRunner) relayRoots() []hostlink.Root {
	// A VM's virtio-fs raises no event for the Mac's edits either (spike 2).
	iso, _, ok := r.a.createdIsolation()
	if !ok || iso != isolationGVisor && iso != isolationVM {
		return nil
	}
	var roots []hostlink.Root
	for _, m := range r.a.mountedRoots() {
		roots = append(roots, hostlink.Root{Host: m.Host, Container: m.Container})
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

// cappedLog is link.log, appended to by every link, so a restart keeps
// what the last one said; whenever a write would take it past max, it is
// moved to path.1 (replacing the one before) and started again, with a
// line saying so.
type cappedLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	n    int64
	max  int64
}

func openCappedLog(path string, max int64) (*cappedLog, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &cappedLog{path: path, f: f, n: fi.Size(), max: max}, nil
}

func (c *cappedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n > 0 && c.n+int64(len(p)) > c.max {
		if err := c.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := c.f.Write(p)
	c.n += int64(n)
	return n, err
}

// rotate moves the log to path.1 and starts a new one. Under c.mu.
func (c *cappedLog) rotate() error {
	if err := os.Rename(c.path, c.path+".1"); err != nil {
		return err
	}
	f, err := os.OpenFile(c.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	c.f.Close()
	c.f, c.n = f, 0
	m, _ := fmt.Fprintf(c.f, "%s caboose link: link.log reached %d MiB: the lines before are in %s.1\n", time.Now().Format("2006/01/02 15:04:05"), c.max>>20, filepath.Base(c.path))
	c.n += int64(m)
	return nil
}

func (c *cappedLog) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.f.Close()
}
