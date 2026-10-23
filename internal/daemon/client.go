package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// How long a client waits for a daemon it started to answer, and for one it
// stopped to let go of its lock.
const (
	// StartWait is longer than the login shell the daemon asks for its
	// environment before it serves (launcher.loginEnvTimeout, 5 s).
	StartWait = 10 * time.Second
	StopWait  = 5 * time.Second
	// StopExecWait is added to StopWait for a daemon that runs host
	// commands when it is asked to stop: its links end each one with
	// SIGTERM, then SIGKILL 3 s later, and what the command's group left
	// gets as long again and 2 s more (hostlink), so a stop with a
	// command that ignores SIGTERM takes longer than StopWait alone.
	StopExecWait = 10 * time.Second
)

// Config is how a client reaches, and starts, the daemon of one directory.
type Config struct {
	Dir     string // the daemon directory (Dir)
	Version string // this caboose's version, for the hello
	Client  string // the client's kind; "" is daemonproto.ClientCLI

	// Spawn starts the daemon, detached (StartDetached), for Ensure.
	Spawn func() error
	// NoReplace has Ensure never replace a running daemon, whatever its
	// protocol: one of an older protocol is returned connected, for the
	// caller to ask to make way itself (the menu-bar app, a background
	// process, asks without force). Ensure starts one only when nothing
	// answers.
	NoReplace bool
	// StartWait bounds the wait for a daemon Spawn started; 0 is StartWait.
	StartWait time.Duration
	// StopWait and StopExecWait bound Stop's wait; 0 is the constant of
	// the name. Only tests set them.
	StopWait, StopExecWait time.Duration
	// Stopping, when set, is told by Stop how many host commands the
	// daemon is ending and how long it waits for them, before it waits:
	// only when there are any.
	Stopping func(hostExecs int, wait time.Duration)
	// Timeouts of the connection; zero ones are daemonproto's defaults.
	DialTimeout, HelloTimeout, RequestTimeout time.Duration
	// Protocol is the protocol the client claims; 0 is
	// daemonproto.ProtocolVersion. Only tests set it.
	Protocol int
}

func (c Config) options() daemonproto.Options {
	kind := c.Client
	if kind == "" {
		kind = daemonproto.ClientCLI
	}
	return daemonproto.Options{Client: kind, Version: c.Version,
		DialTimeout: c.DialTimeout, HelloTimeout: c.HelloTimeout, RequestTimeout: c.RequestTimeout, Protocol: c.Protocol}
}

func (c Config) startWait() time.Duration {
	if c.StartWait > 0 {
		return c.StartWait
	}
	return StartWait
}

// Connect connects to the running daemon, never starting one and creating
// nothing. Nothing answering is a *daemonproto.Error of kind
// KindUnavailable; see daemonproto.Dial for the others.
func Connect(ctx context.Context, cfg Config) (*daemonproto.Client, error) {
	if err := CheckSocket(cfg.Dir); err != nil {
		return nil, err
	}
	return daemonproto.Dial(ctx, Socket(cfg.Dir), cfg.options())
}

// Ensure connects to the daemon, starting it when nothing answers: under
// start.lock, so that commands racing to start it end with one daemon, it
// looks again, then calls cfg.Spawn and waits for the new daemon to answer.
// A daemon of an older protocol is replaced, at once: asked to exit (a
// forced replace, its host commands killed), waited for to let go of its
// lock, and another spawned, all under start.lock; with cfg.NoReplace it
// is returned connected instead. A daemon that is there but does not
// answer (KindTimeout) is not replaced: the caller should say to run
// caboose daemon restart.
func Ensure(ctx context.Context, cfg Config) (*daemonproto.Client, error) {
	c, err := Connect(ctx, cfg)
	switch {
	case cfg.Spawn == nil:
		return c, err
	case err == nil && cfg.keep(c):
		return c, nil
	case err == nil:
		c.Close() // an older protocol's: replaced below
	case daemonproto.Kind(err) != daemonproto.KindUnavailable:
		return nil, err
	}
	if err := prepareDir(cfg.Dir); err != nil {
		return nil, fmt.Errorf("the daemon's directory: %w", err)
	}
	wait := cfg.startWait()
	lk, err := lockWait(ctx, filepath.Join(cfg.Dir, StartLockFile), 2*wait)
	if err != nil {
		return nil, fmt.Errorf("waiting for another command to start the daemon: %w", err)
	}
	defer unlock(lk)
	c, err = Connect(ctx, cfg)
	switch {
	case err == nil && cfg.keep(c):
		return c, nil // another command started it meanwhile
	case err == nil:
		if err := replaceOlder(ctx, cfg, c); err != nil {
			return nil, err
		}
	case daemonproto.Kind(err) != daemonproto.KindUnavailable:
		return nil, err
	}
	if err := cfg.Spawn(); err != nil {
		return nil, fmt.Errorf("starting the daemon: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		c, err := Connect(ctx, cfg)
		if err == nil || daemonproto.Kind(err) != daemonproto.KindUnavailable {
			return c, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the daemon did not answer within %v of being started (its log: %s)", wait, filepath.Join(cfg.Dir, LogFile))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (c Config) stopWait() time.Duration {
	if c.StopWait > 0 {
		return c.StopWait
	}
	return StopWait
}

func (c Config) stopExecWait() time.Duration {
	if c.StopExecWait > 0 {
		return c.StopExecWait
	}
	return StopExecWait
}

// keep reports whether Ensure uses the daemon cl is connected to as it is:
// one of this protocol or a newer one, and with NoReplace any.
func (c Config) keep(cl *daemonproto.Client) bool {
	return c.NoReplace || cl.Compat() != daemonproto.CompatClientNewer
}

// Stop stops the running daemon and waits for it to let go of its lock. It
// reports whether one was running. With no daemon directory it creates
// nothing; with one, at most start.lock. A
// daemon that holds the lock but does not answer is an error naming its
// PID: only a process holding the lock may remove its socket.
func Stop(ctx context.Context, cfg Config) (bool, error) {
	if _, err := os.Lstat(cfg.Dir); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	// Hold start.lock throughout, so that no command starts a daemon
	// while this one probes the lock or waits for it to come free.
	lk, err := lockWait(ctx, filepath.Join(cfg.Dir, StartLockFile), 2*cfg.startWait())
	if err != nil {
		return false, fmt.Errorf("waiting for another command to start the daemon: %w", err)
	}
	defer unlock(lk)
	lock := filepath.Join(cfg.Dir, LockFile)
	if h, err := held(lock); err != nil || !h {
		return false, err
	}
	c, err := Connect(ctx, cfg)
	if err != nil {
		if h, _ := held(lock); !h {
			return false, nil // it went meanwhile
		}
		return false, fmt.Errorf("the daemon does not answer (%v)%s; stop it with kill", err, pidOf(cfg.Dir))
	}
	// The host commands it runs are ended as it stops, which can take
	// longer than the rest: waited for, and said.
	execs := 0
	if st, err := c.Status(ctx); err == nil {
		// Only the commands the shutdown ends and waits for: the stop and
		// restart jobs it leaves alone are not among them.
		execs = max(st.Daemon.HostExecs-st.Daemon.Jobs, 0)
	}
	err = c.Stop(ctx)
	c.Close()
	if err != nil {
		return true, fmt.Errorf("asking the daemon to stop: %w", err)
	}
	wait := cfg.stopWait()
	why := ""
	if execs > 0 {
		wait += cfg.stopExecWait()
		why = fmt.Sprintf(", ending %d host command(s)", execs)
		if cfg.Stopping != nil {
			cfg.Stopping(execs, wait)
		}
	}
	deadline := time.Now().Add(wait)
	for {
		h, err := held(lock)
		if err != nil {
			return true, err
		}
		if !h {
			return true, nil
		}
		if time.Now().After(deadline) {
			return true, fmt.Errorf("the daemon did not stop within %v%s%s", wait, why, pidOf(cfg.Dir))
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// replaceOlder has the daemon c is connected to, of an older protocol, exit
// at once, and waits for it to let go of its lock. It closes c. The caller
// holds start.lock.
func replaceOlder(ctx context.Context, cfg Config, c *daemonproto.Client) error {
	old := c.Daemon()
	_, err := c.Replace(ctx, cfg.Version, true)
	if daemonproto.Kind(err) == daemonproto.KindUnknownMethod {
		// A daemon from before replace: stop is as old as the protocol.
		err = c.Stop(ctx)
	}
	c.Close()
	if err != nil {
		return fmt.Errorf("an older caboose's daemon (caboose %s, protocol %d) would not make way: %w; run: caboose daemon restart",
			daemonproto.Text(old.Version, 64), old.Protocol, err)
	}
	lock := filepath.Join(cfg.Dir, LockFile)
	deadline := time.Now().Add(StopWait)
	for {
		h, err := held(lock)
		if err != nil {
			return err
		}
		if !h {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("an older caboose's daemon (caboose %s) did not exit within %v of being replaced%s",
				daemonproto.Text(old.Version, 64), StopWait, pidOf(cfg.Dir))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// RunningVersions are the caboose versions the daemon of dir says it is,
// creating nothing and starting nothing: its hello's when it answers, and
// daemon.json's while something holds its lock (a daemon that hangs, or
// speaks another protocol, says it there). None when none runs.
func RunningVersions(ctx context.Context, cfg Config) []string {
	var vs []string
	if c, err := Connect(ctx, cfg); err == nil {
		vs = append(vs, c.Daemon().Version)
		c.Close()
	}
	if h, _ := held(filepath.Join(cfg.Dir, LockFile)); h {
		if st, err := ReadState(cfg.Dir); err == nil && st.Version != "" && (len(vs) == 0 || vs[0] != st.Version) {
			vs = append(vs, st.Version)
		}
	}
	return vs
}

func pidOf(dir string) string {
	if st, err := ReadState(dir); err == nil && st.PID > 0 {
		return fmt.Sprintf(" (pid %d)", st.PID)
	}
	return ""
}

// The variables a daemon is started with, beside CABOOSE_HOME: it never
// inherits the terminal that happened to start it.
var minimalVars = []string{"HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "LANG"}

// MinimalEnv is the environment a daemon is started with: HOME, USER,
// LOGNAME, SHELL, TMPDIR and LANG as getenv has them (when set), and
// CABOOSE_HOME as cabooseHome, the one the starting command resolved.
func MinimalEnv(getenv func(string) string, cabooseHome string) []string {
	var env []string
	for _, k := range minimalVars {
		if v := getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return append(env, "CABOOSE_HOME="+cabooseHome)
}

// StartDetached starts exe with args and exactly env, in a session of its
// own, at /, with stdin, stdout and stderr on /dev/null: it outlives the
// command that started it. The caller releases or waits for it.
func StartDetached(exe string, args, env []string) (*exec.Cmd, error) {
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
