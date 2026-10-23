package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/conformance"
	"github.com/bfreis/caboose/internal/docker"
)

// The conformance suite's environment: who the caller is, compared as
// paths only, and the process environment the suite's launcher runs
// with, which is the throwaway environment's alone.

// confFacts are the guard's facts from the caller's environment: the real
// CABOOSE_HOME and home, and every root of every environment of theirs.
func confFacts(getenv config.Env) conformance.Facts {
	home := getenv("HOME")
	ch := getenv("CABOOSE_HOME")
	if ch == "" && home != "" {
		ch = home + "/.caboose"
	}
	return conformance.Facts{
		CabooseHome: ch,
		UserHome:    home,
		RealRoots:   func() ([]string, error) { return realRoots(getenv, ch) },
	}
}

// realRoots are the host paths of every root of every environment in
// cabooseHome, as the launcher's own loader resolves them: the default
// environment and each directory of envs/. Only the paths are kept.
// Anything that cannot be loaded refuses, since a root it would name
// cannot be ruled out.
func realRoots(getenv config.Env, cabooseHome string) ([]string, error) {
	if cabooseHome == "" {
		return nil, errors.New("no CABOOSE_HOME and no HOME")
	}
	names := []string{config.DefaultEnv}
	entries, err := os.ReadDir(filepath.Join(cabooseHome, "envs"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() && config.ValidEnv(e.Name()) && e.Name() != config.DefaultEnv {
			names = append(names, e.Name())
		}
	}
	// Each environment is loaded as the caller would load it, with its
	// name explicit and CABOOSE_DATA_DIR out of the way (it names one
	// environment's data dir, not roots).
	only := func(k string) string {
		switch k {
		case "HOME":
			return getenv("HOME")
		case "CABOOSE_HOME":
			return cabooseHome
		}
		return ""
	}
	var out []string
	for _, n := range names {
		c, err := config.Load(only, config.OSFS{}, n)
		if err != nil {
			return nil, fmt.Errorf("environment %s: %v", n, err)
		}
		for _, r := range c.Roots {
			out = append(out, r.Host)
		}
	}
	return out, nil
}

// confScrub is the process environment the suite runs its launcher with:
// the guard's launch environment, set in place of the caller's, so that
// every child (docker, the link helper, caboose-vmm) inherits it and
// nothing else. It returns the environment now in force, which the guard
// checks.
func confScrub(g *conformance.Guard, environ func() []string, set func([]string) error) ([]string, error) {
	want := g.LaunchEnv(environ())
	if err := set(want); err != nil {
		return nil, err
	}
	got := environ()
	if err := g.CheckLaunchEnv(got); err != nil {
		return nil, err
	}
	return got, nil
}

// setProcessEnv replaces the process's environment with env.
func setProcessEnv(env []string) error {
	os.Clearenv()
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

// confApp is the App the suite drives: its configuration loaded from the
// scrubbed environment alone (environ, which the guard checks first), for
// the throwaway environment by name, so that only B's config.toml is
// read; the engine pinned when the isolation has one.
func confApp(g *conformance.Guard, environ []string, engineHost, checkout string, stdout, stderr io.Writer) (*App, error) {
	if err := g.CheckLaunchEnv(environ); err != nil {
		return nil, err
	}
	getenv := envLookup(environ)
	cfg, err := config.Load(getenv, config.OSFS{}, g.Env())
	if err != nil {
		return nil, err
	}
	switch {
	case cfg.Env != g.Env():
		return nil, fmt.Errorf("conformance: the launcher loaded environment %q", cfg.Env)
	case cfg.CabooseHome != g.CabooseHome():
		return nil, fmt.Errorf("conformance: the launcher's CABOOSE_HOME is %s", cfg.CabooseHome)
	case cfg.DataDir != g.DataDir():
		return nil, fmt.Errorf("conformance: the launcher's data dir is %s", cfg.DataDir)
	case cfg.File == nil || cfg.File.Path != filepath.Join(g.Base(), filepath.FromSlash(confConfigRel(g.Env()))):
		return nil, errors.New("conformance: the launcher did not read the suite's config.toml")
	case !cfg.NoAutoUpdate:
		return nil, errors.New("conformance: the launcher would update itself")
	}
	if err := checkConfConfig(cfg, confSettingsFor(g)); err != nil {
		return nil, err
	}
	if err := cfg.CheckEnv(config.OSFS{}); err != nil {
		return nil, err
	}
	if err := cfg.ResolveRoots(); err != nil {
		return nil, err
	}
	if cfg.Isolation != config.KindVM && engineHost == "" {
		return nil, errors.New("conformance: no engine endpoint was pinned")
	}
	return &App{
		Cfg:      cfg,
		Docker:   &docker.CLI{Path: "docker", Stderr: stderr, Host: engineHost},
		Checkout: checkout,
		Stdout:   stdout,
		Stderr:   stderr,
	}, nil
}

// envLookup is getenv over environ: the last of a name wins, as in a
// process's environment.
func envLookup(environ []string) config.Env {
	m := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return func(k string) string { return m[k] }
}

// swVers is macOS's own, by absolute path: the run's environment is
// scrubbed, and no PATH lookup may pick another.
const swVers = "/usr/bin/sw_vers"

// confOSVersion is the host's OS version for the results header: on
// darwin what `sw_vers -productVersion` prints (through run, bounded),
// trimmed; "" anywhere else or when it fails. The header writes it only
// when it is a plain dotted version.
func confOSVersion(goos string, run func(ctx context.Context, name string, args ...string) ([]byte, error)) string {
	if goos != "darwin" || run == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := run(ctx, swVers, "-productVersion")
	if err != nil || len(out) > 64 {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runOutput runs name with args, its stdout the result.
func runOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
