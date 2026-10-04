package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/hostlink"
)

// The link helper's files, in the data dir itself: the host's, never under
// a directory the container can write.
const (
	linkLockFile = "link.lock"
	linkLogFile  = "link.log"
	// linkStateFile is the running helper's PID and the settings it runs
	// with, for a launch to compare its own with.
	linkStateFile = "link.json"
	// AgentPath is where the layer installs caboose-agent.
	AgentPath = "/usr/local/bin/caboose-agent"
)

// linkRetry bounds the wait between reconnects while the container runs.
const (
	linkRetryMin = time.Second
	linkRetryMax = 30 * time.Second
	// linkConfigEvery is how often the helper looks at config.toml.
	linkConfigEvery = 2 * time.Second
	// linkStopWait is how long a stop waits for the old helper to go.
	linkStopWait = 5 * time.Second
)

// linkSettings are the settings a helper runs with.
type linkSettings struct {
	ForwardPorts string `json:"forward_ports"`
	OpenURLs     string `json:"open_urls"`
}

func settingsOf(c *config.Config) linkSettings {
	return linkSettings{ForwardPorts: c.ForwardPorts, OpenURLs: c.OpenURLs}
}

// check parses the settings, as the helper needs them.
func (s linkSettings) check() (hostlink.PortSet, error) {
	ports, err := hostlink.ParsePorts(s.ForwardPorts)
	if err != nil {
		return nil, err
	}
	switch s.OpenURLs {
	case hostlink.OpenAsk, hostlink.OpenAllow, hostlink.OpenOff:
	default:
		return nil, fmt.Errorf(`open_urls: %q is not "ask", "allow" or "off"`, s.OpenURLs)
	}
	return ports, nil
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
// config.toml itself when it changes.
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
	ports, err := settings.check()
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

	var out io.Writer = a.Stderr
	if background {
		f, err := os.OpenFile(filepath.Join(a.Cfg.DataDir, linkLogFile), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil
		}
		defer f.Close()
		out = f
	}
	r := &linkRunner{a: a, log: log.New(out, "caboose link: ", log.LstdFlags), settings: settings,
		cfg: hostlink.Config{Ports: ports, OpenURL: settings.OpenURLs, Actions: hostlink.System{}}}
	r.cfg.Log = r.log
	r.writeState()
	stop := make(chan struct{})
	defer close(stop)
	go r.watchConfig(stop)
	return r.run(background)
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
	if err := a.spawn(exe, "--env", a.Cfg.Env, "link", "--background"); err != nil {
		return Die("cannot start the link: %v", err)
	}
	if state := a.Docker.ContainerState(a.Cfg.Container); state != "running" {
		a.Note("started a link, but container %s is %s: it stops at once, and the next launch starts one", a.Cfg.Container, state)
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
	wait := linkRetryMin
	for {
		if state := a.Docker.ContainerState(a.Cfg.Container); state != "running" {
			r.log.Printf("container %s is %s: done", a.Cfg.Container, state)
			return nil
		}
		started := time.Now()
		err := r.once()
		var stop *linkStop
		if errors.As(err, &stop) {
			r.log.Print(stop.msg)
			if background {
				return nil
			}
			return Die("%s", stop.msg)
		}
		r.mu.Lock()
		reloaded := r.reloaded
		r.reloaded = false
		r.mu.Unlock()
		if reloaded {
			wait = linkRetryMin
			continue
		}
		r.log.Printf("link ended: %v", err)
		if time.Since(started) > linkRetryMax {
			wait = linkRetryMin
		}
		time.Sleep(wait)
		wait = min(2*wait, linkRetryMax)
	}
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
	ports, err := next.check()
	if err != nil {
		r.log.Printf("config.toml changed, but %v: keeping the settings in use", err)
		return
	}
	r.mu.Lock()
	if next == r.settings {
		r.mu.Unlock()
		return
	}
	r.settings = next
	r.cfg.Ports, r.cfg.OpenURL = ports, next.OpenURLs
	sess := r.sess
	r.reloaded = sess != nil
	r.mu.Unlock()
	r.writeState()
	r.log.Printf("config.toml changed: forward_ports %q, open_urls %q; reconnecting", next.ForwardPorts, next.OpenURLs)
	if sess != nil {
		sess.Close()
	}
}

// linkStop is an ending that retrying cannot fix.
type linkStop struct{ msg string }

func (e *linkStop) Error() string { return e.msg }

// once runs one `docker exec -i ... caboose-agent link` and serves it.
func (r *linkRunner) once() error {
	a := r.a
	cmd := a.Docker.Command("exec", "-i", a.Cfg.Container, AgentPath, "link")
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
