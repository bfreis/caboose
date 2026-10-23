package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/datadir"
)

// daemonWritableBudget is how long a restore waits for the daemon to say
// what the sandboxes reach before working it out itself.
const daemonWritableBudget = 10 * time.Second

// sandboxReach is what every sandbox under CABOOSE_HOME reaches now, read
// off each one as sandboxWritable reads it (running or stopped, as it was
// created): the host paths it can write, and the roots it mounts
// read-only, each with the environments whose sandboxes reach it. An
// environment it cannot ask is an error, never left out.
func (a *App) sandboxReach() (*daemonproto.WritableResult, error) {
	names, err := a.envNames()
	if err != nil {
		return nil, err
	}
	writable, readOnly := map[string][]string{}, map[string][]string{}
	add := func(m map[string][]string, env string, paths []string) {
		for _, p := range paths {
			if !slices.Contains(m[p], env) {
				m[p] = append(m[p], env)
			}
		}
	}
	for _, n := range names {
		b, ok, err := a.sandboxBox(n)
		if err != nil {
			return nil, fmt.Errorf("environment %s: %v", n, err)
		}
		if !ok {
			continue
		}
		w, err := writableOf(b)
		if err != nil {
			return nil, fmt.Errorf("the sandbox of environment %s: %v", n, err)
		}
		ro, err := readOnlyRootsOf(b)
		if err != nil {
			return nil, fmt.Errorf("the sandbox of environment %s: %v", n, err)
		}
		add(writable, n, w)
		add(readOnly, n, ro)
	}
	return &daemonproto.WritableResult{Writable: envPaths(writable), ReadOnly: envPaths(readOnly)}, nil
}

// envPaths is m as a sorted list.
func envPaths(m map[string][]string) []daemonproto.WritablePath {
	out := []daemonproto.WritablePath{}
	for p, envs := range m {
		sort.Strings(envs)
		out = append(out, daemonproto.WritablePath{Path: p, Envs: envs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// readOnlyRootsOf are the roots sandbox b mounts without writing all of
// them, as host paths: what it can read there (the writable parts inside
// such a root are mounts of their own, which writableOf counts). No
// sandbox, or one whose labels name no roots, none.
func readOnlyRootsOf(b backend.Backend) ([]string, error) {
	labels, err := b.Labels()
	if err != nil {
		if noSandbox(err) {
			return nil, nil
		}
		return nil, err
	}
	var roots []string
	if json.Unmarshal([]byte(labels[assets.LabelRoots]), &roots) != nil {
		return nil, nil
	}
	mounts, err := b.Mounts()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range mounts {
		if !slices.Contains(roots, m.Target) || slices.Equal(mountedWritable(labels, m.Target), []string{"."}) {
			continue
		}
		if p := physicalOr(m.Source); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

var _ daemon.Writable = (*DaemonLinks)(nil)

// Writable is the writable method (daemon.Writable): what the sandboxes
// reach, read off them now, on the App of the first environment under
// CABOOSE_HOME whose configuration loads. None at all is no sandbox; some,
// none of which loads, is an error.
func (l *DaemonLinks) Writable(context.Context) (*daemonproto.WritableResult, error) {
	entries, err := os.ReadDir(filepath.Join(l.home, "envs"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !config.ValidEnv(e.Name()) {
			continue
		}
		app, err := l.newApp(e.Name())
		if err != nil {
			errs = append(errs, fmt.Errorf("environment %s: %v", e.Name(), err))
			continue
		}
		return app.sandboxReach()
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &daemonproto.WritableResult{Writable: []daemonproto.WritablePath{}, ReadOnly: []daemonproto.WritablePath{}}, nil
}

// writableCaller is a daemon client that answers writable.
type writableCaller interface {
	Writable(ctx context.Context) (*daemonproto.WritableResult, error)
}

// restorePaths is restore's checkpoint.SandboxPaths: what the daemon says
// the sandboxes reach, with what every environment's configuration would
// mount (configuredPaths), and when the daemon cannot say, the
// conservative set (conservativePaths). Why gives the reason the daemon
// was not used, and notes what either had to leave out.
type restorePaths struct {
	a     *App
	apps  func(env string) (*App, error)
	why   string
	notes []string
}

func (p *restorePaths) SandboxPaths(ctx context.Context) (checkpoint.PathSet, error) {
	set, err := p.a.daemonPaths(ctx)
	if err == nil {
		p.notes = p.a.configuredPaths(p.apps, &set)
		return set, nil
	}
	p.why = daemonErrText(err)
	set, p.notes = p.a.conservativePaths(p.apps)
	return set, nil
}

// daemonPaths asks the daemon, which it never starts, what the sandboxes
// reach.
func (a *App) daemonPaths(ctx context.Context) (checkpoint.PathSet, error) {
	ctx, cancel := context.WithTimeout(ctx, daemonWritableBudget)
	defer cancel()
	c, err := a.dialDaemon(ctx, false)
	if err != nil {
		return checkpoint.PathSet{}, err
	}
	defer c.Close()
	wc, ok := c.(writableCaller)
	if !ok {
		return checkpoint.PathSet{}, errors.New("the daemon cannot say what the sandboxes write")
	}
	r, err := wc.Writable(ctx)
	if err != nil {
		return checkpoint.PathSet{}, err
	}
	if err := r.Validate(); err != nil {
		return checkpoint.PathSet{}, err
	}
	var set checkpoint.PathSet
	for _, p := range r.Writable {
		for _, e := range p.Envs {
			set.Writable = append(set.Writable, checkpoint.EnvPath{Env: e, Path: p.Path})
		}
	}
	for _, p := range r.ReadOnly {
		for _, e := range p.Envs {
			set.ReadOnly = append(set.ReadOnly, checkpoint.EnvPath{Env: e, Path: p.Path})
		}
	}
	return set, nil
}

// conservativePaths is what the sandboxes reach when the daemon cannot
// say, worked out without it, counting more rather than less: every root
// of every environment with a directory under CABOOSE_HOME (envsWithDir),
// as configured and as its sandbox mounts it, counted wholly writable,
// and every mount its sandbox has, and every data dir's home/. Nothing is read-only. apps makes
// another environment's App (nil: envApp). What it could not look at is
// left out and said in notes; it never fails, since a restore is most
// needed when things are broken.
func (a *App) conservativePaths(apps func(env string) (*App, error)) (checkpoint.PathSet, []string) {
	if apps == nil {
		apps = a.envApp
	}
	set := checkpoint.PathSet{Fallback: true}
	var notes []string
	seen := map[checkpoint.EnvPath]bool{}
	add := func(env, p string) {
		if p == "" {
			return
		}
		ep := checkpoint.EnvPath{Env: env, Path: physicalOr(p)}
		if !seen[ep] {
			seen[ep] = true
			set.Writable = append(set.Writable, ep)
		}
	}
	names, err := a.envsWithDir()
	if err != nil {
		notes = append(notes, fmt.Sprintf("the environments could not be listed (%v): only %s's roots are counted", err, a.Cfg.Env))
		names = []string{a.Cfg.Env}
	}
	for _, n := range names {
		b := a
		if n != a.Cfg.Env {
			if b, err = apps(n); err != nil {
				notes = append(notes, fmt.Sprintf("environment %s's configuration cannot be read (%v): its roots are not counted", n, err))
				continue
			}
		}
		for _, r := range b.Cfg.Roots {
			add(n, r.Host)
		}
		for _, r := range b.mountedRoots() {
			add(n, r.Host)
		}
		if mounts, err := sandboxMounts(b.box()); err != nil {
			notes = append(notes, fmt.Sprintf("the sandbox of environment %s cannot say what it mounts (%v): only its roots and home are counted", n, err))
		} else {
			for _, m := range mounts {
				add(n, m.Source)
			}
		}
		if b.Cfg.DataDir != "" {
			add(n, filepath.Join(b.Cfg.DataDir, datadir.HomeDir))
		}
	}
	return set, notes
}

// configuredPaths adds to set what every environment with a directory
// under CABOOSE_HOME (envsWithDir) would mount from its configured roots,
// sandbox or not, as rootMounts plans it: each writable part (a wholly
// writable root is all of it) to Writable, each read-only root to
// ReadOnly. A root configured but not mounted yet is one a restart hands
// the sandbox, so a target there is judged as if it were. apps makes
// another environment's App (nil: envApp); what it could not look at is
// said in the notes it returns.
func (a *App) configuredPaths(apps func(env string) (*App, error), set *checkpoint.PathSet) []string {
	if apps == nil {
		apps = a.envApp
	}
	var notes []string
	seen := map[checkpoint.EnvPath]bool{}
	for _, p := range set.Writable {
		seen[p] = true
	}
	ro := map[checkpoint.EnvPath]bool{}
	for _, p := range set.ReadOnly {
		ro[p] = true
	}
	names, err := a.envsWithDir()
	if err != nil {
		notes = append(notes, fmt.Sprintf("the environments could not be listed (%v): only %s's configured roots are counted", err, a.Cfg.Env))
		names = []string{a.Cfg.Env}
	}
	for _, n := range names {
		b := a
		if n != a.Cfg.Env {
			if b, err = apps(n); err != nil {
				notes = append(notes, fmt.Sprintf("environment %s's configuration cannot be read (%v): its configured roots are not counted", n, err))
				continue
			}
		}
		for _, m := range rootMounts(b.Cfg.Roots) {
			ep := checkpoint.EnvPath{Env: n, Path: physicalOr(m.Source)}
			switch {
			case ep.Path == "":
			case m.ReadOnly && !ro[ep]:
				ro[ep] = true
				set.ReadOnly = append(set.ReadOnly, ep)
			case !m.ReadOnly && !seen[ep]:
				seen[ep] = true
				set.Writable = append(set.Writable, ep)
			}
		}
	}
	return notes
}
