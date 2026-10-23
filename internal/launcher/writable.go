package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
)

// aliasTmp is where aliases with no cwd of their own run, each in a fresh
// directory of its own: in the daemon directory, which no sandbox mounts.
func aliasTmp(daemonDir string) string { return filepath.Join(daemonDir, "alias-tmp") }

// sandboxWritable is every host path some sandbox under CABOOSE_HOME can
// write now, physical: what decides whether an alias's command is the
// sandbox's to change. It is read off each environment's sandbox, running
// or stopped, as it was created (sandboxBox), never off config.toml,
// which may have changed since: a path one sandbox can write counts for
// every environment's policy. An environment it cannot ask is an error,
// which the broker takes as everything writable.
func (a *App) sandboxWritable() ([]string, error) {
	names, err := a.envNames()
	if err != nil {
		return nil, err
	}
	var all []string
	for _, n := range names {
		w, err := a.envWritable(n)
		if err != nil {
			return nil, err
		}
		for _, p := range w {
			if !slices.Contains(all, p) {
				all = append(all, p)
			}
		}
	}
	return all, nil
}

// envWritable is what environment env's sandbox can write now
// (writableOf): no paths for an environment that may have no sandbox. An
// environment it cannot ask is an error.
func (a *App) envWritable(env string) ([]string, error) {
	b, ok, err := a.sandboxBox(env)
	if err != nil {
		return nil, fmt.Errorf("environment %s: %v", env, err)
	}
	if !ok {
		return nil, nil
	}
	w, err := writableOf(b)
	if err != nil {
		return nil, fmt.Errorf("the sandbox of environment %s: %v", env, err)
	}
	return w, nil
}

// sandboxBox is environment env's sandbox, and whether it may have one: an
// environment with no directory under CABOOSE_HOME has none.
func (a *App) sandboxBox(env string) (backend.Backend, bool, error) {
	if a.envBox != nil {
		return a.envBox(env)
	}
	if env == a.Cfg.Env {
		if a.Backend == nil && !a.isVM() && a.Docker == nil {
			return nil, false, errors.New("no docker to ask about its container")
		}
		return a.box(), true, nil
	}
	if _, err := os.Stat(config.EnvDirFor(a.Cfg.CabooseHome, env)); errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	cfg, err := config.Load(a.Cfg.Getenv, config.OSFS{}, env)
	if err != nil {
		return nil, false, err
	}
	other := &App{Cfg: cfg, Docker: a.Docker, Checkout: a.Checkout, Stdout: io.Discard, Stderr: io.Discard}
	if !other.isVM() && a.Docker == nil {
		return nil, false, errors.New("no docker to ask about its container")
	}
	return other.box(), true, nil
}

// writableOf is what sandbox b can write, as host paths, physical where
// they resolve: every mount it has but the roots it mounts read-only
// (LabelRoots, LabelWritable), whose writable parts are mounts of their
// own. caboose's other read-only mounts count as writable all the same:
// nothing an alias runs lives there, and counting more is the safe way to
// be wrong. A root of
// a sandbox whose labels do not say counts as writable (mountedWritable),
// failing closed.
// No sandbox, nothing; a backend that cannot say whether there is one (an
// engine that does not answer) is an error, never nothing. Under vm a file
// is shared as its whole directory, so the directory counts.
func writableOf(b backend.Backend) ([]string, error) {
	labels, err := b.Labels()
	if err != nil {
		if noSandbox(err) {
			return nil, nil
		}
		return nil, err
	}
	mounts, err := b.Mounts()
	if err != nil {
		return nil, err
	}
	var roots []string
	if json.Unmarshal([]byte(labels[assets.LabelRoots]), &roots) != nil {
		roots = nil // every mount is writable, then
	}
	var out []string
	for _, m := range mounts {
		if slices.Contains(roots, m.Target) && !slices.Equal(mountedWritable(labels, m.Target), []string{"."}) {
			continue
		}
		p := physicalOr(m.Source)
		if _, vm := b.(*backend.VM); vm {
			if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
				p = filepath.Dir(p)
			}
		}
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// noSandbox reports whether err, a backend's failure to read a sandbox's
// labels, says there is no such sandbox: a VM with no state, docker's "No
// such container". Anything else (the engine down, a timeout) is not
// knowing.
func noSandbox(err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var f *docker.Failure
	return errors.As(err, &f) && strings.Contains(strings.ToLower(f.Stderr), "no such")
}
