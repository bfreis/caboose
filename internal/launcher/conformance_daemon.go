package launcher

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/daemon"
)

// The conformance suite's daemon: the throwaway CABOOSE_HOME's own, which
// serves the environment's link, checkpoints and broker as for
// any environment. The suite starts it itself, through the App's Spawn
// seam that every daemon start goes through (daemon.Ensure, startDaemon),
// with an environment of its own: the stubs first on its PATH, no SHELL
// (so the daemon keeps that PATH rather than ask a login shell for the
// user's: LoginEnv), the pinned engine as DOCKER_HOST, and the suite's
// environment canary, which no alias may see.

// confDaemon starts and identifies the suite's daemon.
type confDaemon struct {
	g   *conformance.Guard
	env []string
	// start starts exe detached with args and exactly env, and returns
	// its PID; daemon.StartDetached, a seam for the tests.
	start func(exe string, args, env []string) (int, error)

	mu  sync.Mutex
	pid int // what start returned; 0 before
}

func newConfDaemon(g *conformance.Guard, environ []string, engine string) *confDaemon {
	return &confDaemon{g: g, env: confDaemonEnv(g, environ, engine), start: confStartDetached}
}

func confStartDetached(exe string, args, env []string) (int, error) {
	cmd, err := daemon.StartDetached(exe, args, env)
	if err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	// Not reaped, as startDaemon's: the teardown stops it through its socket.
	return pid, cmd.Process.Release()
}

// confEnvCanary is the variable the suite exports in its daemon's
// environment, and so its links', which no alias may see.
func confEnvCanary(g *conformance.Guard) string {
	return "CONFORMANCE_CANARY_" + strings.ToUpper(g.Nonce8())
}

// confDaemonEnv is the daemon's environment: daemon.MinimalEnv's
// variables from the scrubbed environ but SHELL, CABOOSE_HOME as B/home,
// CABOOSE_NO_AUTO_UPDATE=1, PATH with the stubs first, DOCKER_HOST as the
// pinned engine (none under vm), and the environment canary.
func confDaemonEnv(g *conformance.Guard, environ []string, engine string) []string {
	get := envLookup(environ)
	env := daemon.MinimalEnv(func(k string) string {
		if k == "SHELL" {
			return ""
		}
		return get(k)
	}, g.CabooseHome())
	env = append(env, "CABOOSE_NO_AUTO_UPDATE=1", "PATH="+g.StubPath()+":"+or(get("PATH"), fallbackPath))
	if engine != "" {
		env = append(env, "DOCKER_HOST="+engine)
	}
	return append(env, confEnvCanary(g)+"=caboose-conformance-"+g.Nonce())
}

// spawn is the App's Spawn: the daemon alone, once.
func (d *confDaemon) spawn(exe string, args ...string) error {
	if !slices.Equal(args, []string{"daemon"}) {
		return fmt.Errorf("conformance: the suite starts no %q", strings.Join(args, " "))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pid != 0 {
		return fmt.Errorf("conformance: the suite's daemon (pid %d) was started already", d.pid)
	}
	pid, err := d.start(exe, args, d.env)
	if err != nil {
		return err
	}
	d.pid = pid
	return nil
}

func (d *confDaemon) spawned() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pid
}

// stubsFirst is nil when the daemon was started with the stubs' directory
// first on its PATH and no SHELL to replace that PATH.
func (d *confDaemon) stubsFirst() error {
	get := envLookup(d.env)
	if p := get("PATH"); !strings.HasPrefix(p, d.g.StubPath()+":") {
		return errors.New("the daemon's PATH does not begin with the stubs' directory")
	}
	if slices.ContainsFunc(d.env, func(kv string) bool { return strings.HasPrefix(kv, "SHELL=") }) {
		return errors.New("the daemon is given a SHELL, whose login PATH it would take")
	}
	if d.spawned() == 0 {
		return errors.New("the suite has not started the daemon")
	}
	return nil
}

// check is nil when the daemon a answers is the one the suite started:
// its directory is a's daemon directory, inside the throwaway
// CABOOSE_HOME (daemon-dev for a checkout's build), and its PID the one
// spawn started.
func (d *confDaemon) check(ctx context.Context, a *App) error {
	want := a.daemonDir()
	if !confInside(want, d.g.CabooseHome()) {
		return fmt.Errorf("conformance: the daemon's directory %s is not in %s", want, d.g.CabooseHome())
	}
	c, err := a.dialDaemon(ctx, false)
	if err != nil {
		return fmt.Errorf("conformance: the suite's daemon does not answer: %v", err)
	}
	defer c.Close()
	st, err := c.Status(ctx)
	if err != nil {
		return fmt.Errorf("conformance: the suite's daemon: %v", err)
	}
	switch pid := d.spawned(); {
	case pid == 0:
		return errors.New("conformance: a daemon answers that the suite did not start")
	case st.Daemon.PID != pid:
		return fmt.Errorf("conformance: the daemon that answers is pid %d, not the suite's %d", st.Daemon.PID, pid)
	case st.Daemon.Dir != "" && filepath.Clean(st.Daemon.Dir) != want:
		return fmt.Errorf("conformance: the daemon that answers is in %s, not %s", st.Daemon.Dir, want)
	}
	return nil
}

// confStopDaemon stops the suite's daemon, which ends the environment's
// link and every host command it ran, and confirms no link holds the data
// dir: only a daemon whose directory is in the throwaway CABOOSE_HOME.
func confStopDaemon(g *conformance.Guard, a *App) error {
	dir := a.daemonDir()
	if !confInside(dir, g.CabooseHome()) {
		return fmt.Errorf("conformance: the daemon's directory %s is not in %s", dir, g.CabooseHome())
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonLinkBudget)
	defer cancel()
	if _, err := daemon.Stop(ctx, a.daemonConfig()); err != nil {
		return err
	}
	if held, err := linkRunning(a.Cfg.DataDir); err != nil || held {
		return fmt.Errorf("conformance: a link still holds %s (%v)", a.Cfg.DataDir, err)
	}
	return nil
}

// confInside reports whether p is strictly inside dir, both absolute.
func confInside(p, dir string) bool {
	if !filepath.IsAbs(p) || !filepath.IsAbs(dir) {
		return false
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
