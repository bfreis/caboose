package launcher

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/docker"
)

// ckEnv is a CABOOSE_HOME with one environment, a root of host
// directories, and an App on a fake sandbox, all under t.TempDir().
type ckEnv struct {
	tmp, home, envDir, dataDir, dev, tools string
	box                                    *backendtest.Fake
	app                                    *App
}

func ckMkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func ckWriteFile(t *testing.T, p, body string) {
	t.Helper()
	ckMkdirs(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func phys(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newCkEnv(t *testing.T, paths ...string) *ckEnv {
	t.Helper()
	tmp := physicalTempDir(t)
	e := &ckEnv{tmp: tmp, home: filepath.Join(tmp, "caboose"), dev: filepath.Join(tmp, "dev"), tools: filepath.Join(tmp, "tools")}
	e.envDir = config.EnvDirFor(e.home, config.DefaultEnv)
	e.dataDir = filepath.Join(e.envDir, "data")
	ckMkdirs(t, e.dataDir, filepath.Join(e.dataDir, "home", ".claude", "projects"),
		filepath.Join(e.dev, "proj", "sub"), filepath.Join(e.dev, "other"), e.tools)
	ckWriteFile(t, filepath.Join(e.dataDir, "home", ".claude.json"), "{}")
	e.box = &backendtest.Fake{}
	e.app = &App{Cfg: &config.Config{
		Env: config.DefaultEnv, CabooseHome: e.home, EnvDir: e.envDir, DataDir: e.dataDir,
		Container: config.ContainerFor(config.DefaultEnv), Home: tmp, Getenv: func(string) string { return "" },
		Roots: []config.Root{
			{Name: "dev", Host: e.dev, Container: "/work/dev", Writable: []string{"proj"}},
			{Name: "tools", Host: e.tools, Container: "/work/tools", Writable: []string{"."}},
		},
		Checkpoints: paths,
	}, Backend: e.box, Stdout: io.Discard, Stderr: io.Discard}
	return e
}

// resolve resolves paths, by Path.
func (e *ckEnv) resolve(t *testing.T, paths ...string) (map[string]checkpoint.Source, map[string]string) {
	t.Helper()
	e.app.Cfg.Checkpoints = paths
	srcs, bad := e.app.checkpointSources()
	got, why := map[string]checkpoint.Source{}, map[string]string{}
	for _, s := range srcs {
		got[s.Path] = s
	}
	for _, u := range bad {
		why[u.Path] = u.Reason
	}
	if len(got)+len(why) != len(paths) {
		t.Fatalf("%d sources and %d unresolved for %d paths", len(got), len(why), len(paths))
	}
	return got, why
}

func wantSource(t *testing.T, got map[string]checkpoint.Source, why map[string]string, p string, want checkpoint.Source) {
	t.Helper()
	s, ok := got[p]
	if !ok {
		t.Errorf("%s unresolved (%s), want %+v", p, why[p], want)
		return
	}
	want.Path = p
	if s != want {
		t.Errorf("%s: %+v, want %+v", p, s, want)
	}
}

func wantUnresolved(t *testing.T, got map[string]checkpoint.Source, why map[string]string, p, reason string) {
	t.Helper()
	if s, ok := got[p]; ok {
		t.Errorf("%s resolved to %+v, want unresolved (%q)", p, s, reason)
		return
	}
	if !strings.Contains(why[p], reason) {
		t.Errorf("%s: reason %q, want %q", p, why[p], reason)
	}
}

func TestCheckpointSourcesRoots(t *testing.T) {
	e := newCkEnv(t)
	got, why := e.resolve(t, "/work/dev/proj", "/work/dev/proj/sub", "/work/tools", "/work/dev/other", "/work/dev", "/opt/x", "/work/devx", "/work/dev/proj/missing")
	dev, tools := phys(t, e.dev), phys(t, e.tools)
	wantSource(t, got, why, "/work/dev/proj", checkpoint.Source{Kind: checkpoint.SourceRoot, Top: dev, Rel: "proj"})
	wantSource(t, got, why, "/work/dev/proj/sub", checkpoint.Source{Kind: checkpoint.SourceRoot, Top: dev, Rel: "proj/sub"})
	wantSource(t, got, why, "/work/tools", checkpoint.Source{Kind: checkpoint.SourceRoot, Top: tools, Rel: ""})
	for _, p := range []string{"/work/dev/other", "/work/dev", "/opt/x", "/work/devx"} {
		wantUnresolved(t, got, why, p, "is not a writable part of a root")
	}
	wantUnresolved(t, got, why, "/work/dev/proj/missing", "does not exist")
}

// A part the configuration has made read-only is still copied while the
// running sandbox can write it; a part made writable is copied before the
// restart that mounts it so.
func TestCheckpointSourcesActualWritable(t *testing.T) {
	e := newCkEnv(t)
	e.box.Status = "running"
	e.box.SandboxMounts = []backend.Mount{{Source: e.dev, Target: "/work/dev"}, {Source: e.tools, Target: "/work/tools"}}
	dev := phys(t, e.dev)

	// The sandbox was created with all of dev writable; the config has
	// since narrowed it to proj.
	e.box.SandboxLabels = map[string]string{
		assets.LabelRoots:    `["/work/dev","/work/tools"]`,
		assets.LabelWritable: `{"/work/dev":["."],"/work/tools":[]}`,
	}
	got, why := e.resolve(t, "/work/dev/other", "/work/dev/proj", "/work/tools")
	wantSource(t, got, why, "/work/dev/other", checkpoint.Source{Kind: checkpoint.SourceRoot, Top: dev, Rel: "other"})
	wantSource(t, got, why, "/work/dev/proj", checkpoint.Source{Kind: checkpoint.SourceRoot, Top: dev, Rel: "proj"})
	// tools is writable by the configuration only: copied all the same.
	wantSource(t, got, why, "/work/tools", checkpoint.Source{Kind: checkpoint.SourceRoot, Top: phys(t, e.tools), Rel: ""})

	// A sandbox without the label wrote every root it mounts.
	delete(e.box.SandboxLabels, assets.LabelWritable)
	got, why = e.resolve(t, "/work/dev/other")
	wantSource(t, got, why, "/work/dev/other", checkpoint.Source{Kind: checkpoint.SourceRoot, Top: dev, Rel: "other"})

	// Read-only by both accounts: not copied.
	e.box.SandboxLabels[assets.LabelWritable] = `{"/work/dev":["proj"],"/work/tools":["."]}`
	got, why = e.resolve(t, "/work/dev/other")
	wantUnresolved(t, got, why, "/work/dev/other", "not a writable part")

	// A root the running sandbox mounts that the configuration has
	// dropped: what the sandbox writes is copied, from where it is mounted.
	other := filepath.Join(e.tmp, "gone")
	ckMkdirs(t, filepath.Join(other, "p"))
	e.box.SandboxMounts = append(e.box.SandboxMounts, backend.Mount{Source: other, Target: "/work/gone"})
	e.box.SandboxLabels[assets.LabelRoots] = `["/work/dev","/work/tools","/work/gone"]`
	e.box.SandboxLabels[assets.LabelWritable] = `{"/work/dev":["proj"],"/work/tools":["."],"/work/gone":["p"]}`
	got, why = e.resolve(t, "/work/gone/p")
	wantSource(t, got, why, "/work/gone/p", checkpoint.Source{Kind: checkpoint.SourceRoot, Top: phys(t, other), Rel: "p"})
}

func TestCheckpointSourcesHome(t *testing.T) {
	e := newCkEnv(t)
	data := phys(t, e.dataDir)
	ckMkdirs(t, filepath.Join(e.dataDir, "home", ".npm"), filepath.Join(e.dataDir, "home", ".cache"))
	e.box.Status = "running"
	e.box.SandboxMounts = []backend.Mount{{Source: filepath.Join(e.dataDir, "home", ".npm"), Target: "/home/agent/.npm"}}
	got, why := e.resolve(t, "~/.claude", "~/.claude/projects", "~/.claude.json", "~/.npm", "~/.cache", "~/.claude/missing")
	wantSource(t, got, why, "~/.claude", checkpoint.Source{Kind: checkpoint.SourceHome, Top: data, Rel: "home/.claude"})
	wantSource(t, got, why, "~/.claude/projects", checkpoint.Source{Kind: checkpoint.SourceHome, Top: data, Rel: "home/.claude/projects"})
	wantSource(t, got, why, "~/.claude.json", checkpoint.Source{Kind: checkpoint.SourceHome, Top: data, Rel: "home/.claude.json", File: true})
	// Kept because the sandbox mounts it, though its config does not list it.
	wantSource(t, got, why, "~/.npm", checkpoint.Source{Kind: checkpoint.SourceHome, Top: data, Rel: "home/.npm"})
	wantUnresolved(t, got, why, "~/.cache", "not kept by the sandbox config")
	wantUnresolved(t, got, why, "~/.claude/missing", "does not exist")
}

// The sandbox writes its config: an entry in it decides only which of the
// data dir's home/ paths qualify, and never names a host path.
func TestCheckpointSourcesSandboxConfigNamesNoHostPath(t *testing.T) {
	e := newCkEnv(t)
	outside := filepath.Join(e.tmp, "outside")
	ckMkdirs(t, filepath.Join(outside, "secret"), filepath.Join(e.dataDir, "home", ".npm"))
	ckWriteFile(t, filepath.Join(e.dataDir, datadir.SandboxConfig),
		"[[keep]]\npath = \"~/.npm\"\n\n[[keep]]\npath = \""+outside+"\"\n\n[[keep]]\npath = \"~/../../"+strings.TrimPrefix(outside, "/")+"\"\n")
	got, why := e.resolve(t, "~/.npm", outside, "~/"+strings.TrimPrefix(outside, "/"))
	data := phys(t, e.dataDir)
	wantSource(t, got, why, "~/.npm", checkpoint.Source{Kind: checkpoint.SourceHome, Top: data, Rel: "home/.npm"})
	wantUnresolved(t, got, why, outside, "not a writable part of a root")
	wantUnresolved(t, got, why, "~/"+strings.TrimPrefix(outside, "/"), "not kept")
	for _, s := range got {
		if s.Kind == checkpoint.SourceHome && !strings.HasPrefix(s.Host(), filepath.Join(data, "home")+"/") {
			t.Errorf("home source %s at %s, outside the data dir's home", s.Path, s.Host())
		}
	}
}

func TestCheckpointSourcesSymlink(t *testing.T) {
	e := newCkEnv(t)
	outside := filepath.Join(e.tmp, "outside")
	ckMkdirs(t, filepath.Join(outside, "y"), filepath.Join(e.dev, "proj"))
	if err := os.Symlink(outside, filepath.Join(e.dataDir, "home", ".claude", "x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.dev, "proj", "lnk")); err != nil {
		t.Fatal(err)
	}
	got, why := e.resolve(t, "~/.claude/x/y", "~/.claude/x", "/work/dev/proj/lnk/y", "/work/dev/proj/lnk")
	for p := range why {
		wantUnresolved(t, got, why, p, "symbolic link")
	}
	if len(got) != 0 {
		t.Errorf("resolved %v", got)
	}
}

// No checkpoint copies a store or a daemon directory, nor what holds one,
// whatever root reaches it, and whatever name the root gives it.
func TestCheckpointSourcesRefuseTheStore(t *testing.T) {
	e := newCkEnv(t)
	// CABOOSE_HOME is reached through a symlink: identity, not the
	// spelling, decides.
	real := e.home
	e.home = filepath.Join(e.tmp, "home-link")
	if err := os.Symlink(real, e.home); err != nil {
		t.Fatal(err)
	}
	e.app.Cfg.CabooseHome = e.home
	e.app.Cfg.EnvDir = config.EnvDirFor(e.home, config.DefaultEnv)
	ckMkdirs(t, filepath.Join(real, "envs", "default", "checkpoints", "k"), filepath.Join(real, "daemon", "d"),
		filepath.Join(real, "envs", "other", "data"))
	e.app.Cfg.Roots = append(e.app.Cfg.Roots, config.Root{Name: "all", Host: e.tmp, Container: "/work/all", Writable: []string{"."}})
	got, why := e.resolve(t,
		"/work/all",                                    // holds CABOOSE_HOME
		"/work/all/caboose",                            // CABOOSE_HOME
		"/work/all/caboose/envs/default",               // holds the store
		"/work/all/caboose/envs/default/checkpoints",   // the store
		"/work/all/caboose/envs/default/checkpoints/k", // inside it
		"/work/all/caboose/envs/other",                 // holds a store not made yet
		"/work/all/caboose/daemon/d",                   // inside a daemon dir
		"/work/all/caboose/envs/default/data",          // the data dir: none of these
		"/work/all/dev",
	)
	for _, p := range []string{"/work/all", "/work/all/caboose", "/work/all/caboose/envs/default", "/work/all/caboose/envs/default/checkpoints", "/work/all/caboose/envs/other"} {
		wantUnresolved(t, got, why, p, "is or holds the ")
	}
	wantUnresolved(t, got, why, "/work/all/caboose/envs/default/checkpoints/k", "is inside the checkpoint store")
	wantUnresolved(t, got, why, "/work/all/caboose/daemon/d", "is inside the daemon directory")
	tmp := phys(t, e.tmp)
	wantSource(t, got, why, "/work/all/caboose/envs/default/data", checkpoint.Source{Kind: checkpoint.SourceRoot, Top: tmp, Rel: "caboose/envs/default/data"})
	wantSource(t, got, why, "/work/all/dev", checkpoint.Source{Kind: checkpoint.SourceRoot, Top: tmp, Rel: "dev"})
}

// errMounts is a sandbox that exists but cannot say what it mounts.
type errMounts struct{ *backendtest.Fake }

func (errMounts) Mounts() ([]backend.Mount, error) { return nil, errors.New("engine not answering") }

func TestCheckpointExposed(t *testing.T) {
	e := newCkEnv(t)
	ckMkdirs(t, filepath.Join(e.home, "envs", "other"))
	otherBox := &backendtest.Fake{}
	otherCfg := &config.Config{Env: "other", CabooseHome: e.home, EnvDir: config.EnvDirFor(e.home, "other"),
		DataDir: filepath.Join(config.EnvDirFor(e.home, "other"), "data")}
	var otherErr error
	other := &App{Cfg: otherCfg, Backend: otherBox, Stdout: io.Discard, Stderr: io.Discard}
	var asked []string
	apps := func(env string) (*App, error) {
		asked = append(asked, env)
		if otherErr != nil {
			return nil, otherErr
		}
		return other, nil
	}
	s, err := e.app.openCheckpointStore(apps)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Exposed == nil {
		t.Fatal("no exposed check set")
	}
	if err := s.Exposed(); err != nil {
		t.Fatalf("nothing reaches the store: %v", err)
	}
	if len(asked) != 1 || asked[0] != "other" {
		t.Errorf("asked %v, want [other]", asked)
	}

	reached := func(what string) {
		t.Helper()
		err := s.Exposed()
		if !errors.Is(err, checkpoint.ErrExposed) {
			t.Errorf("%s: %v, want ErrExposed", what, err)
		}
	}
	// A root of this environment over CABOOSE_HOME.
	e.app.Cfg.Roots = append(e.app.Cfg.Roots, config.Root{Name: "all", Host: e.tmp, Container: "/work/all"})
	reached("this environment's root")
	e.app.Cfg.Roots = e.app.Cfg.Roots[:2]

	// Another environment's configured root, named through a symlink.
	alias := filepath.Join(e.tmp, "alias")
	if err := os.Symlink(e.envDir, alias); err != nil {
		t.Fatal(err)
	}
	otherCfg.Roots = []config.Root{{Name: "x", Host: alias, Container: "/work/x"}}
	reached("another environment's root, through a symlink")
	otherCfg.Roots = nil

	// A mount of another environment's sandbox, read-only or not.
	otherBox.Status = "exited"
	otherBox.SandboxMounts = []backend.Mount{{Source: e.home, Target: "/work/h"}}
	reached("another sandbox's mount")
	otherBox.SandboxMounts = []backend.Mount{{Source: filepath.Join(s.Dir(), "k"), Target: "/work/k"}}
	ckMkdirs(t, filepath.Join(s.Dir(), "k"))
	reached("a mount inside the store")
	otherBox.SandboxMounts = []backend.Mount{{Source: e.dev, Target: "/work/dev"}}
	if err := s.Exposed(); err != nil {
		t.Errorf("an unrelated mount: %v", err)
	}

	// This environment's own sandbox.
	e.box.Status = "running"
	e.box.SandboxMounts = []backend.Mount{{Source: e.envDir, Target: "/work/e"}}
	reached("this sandbox's mount")
	e.box.SandboxMounts = nil

	// Failing closed: an environment that cannot be read, or a sandbox
	// that cannot say what it mounts.
	otherErr = errors.New("config.toml: bad")
	if err := s.Exposed(); err == nil || !strings.Contains(err.Error(), "environment other") {
		t.Errorf("unreadable environment: %v", err)
	}
	otherErr = nil
	other.Backend = errMounts{otherBox}
	if err := s.Exposed(); err == nil || !strings.Contains(err.Error(), "cannot say what it mounts") {
		t.Errorf("mounts unknown: %v", err)
	}
}

// The data dir's home is reached by every environment's sandbox (its keep
// entries are mounted from it), so a data dir holding the store exposes it.
func TestCheckpointReachDataHome(t *testing.T) {
	e := newCkEnv(t)
	reach, err := e.app.checkpointReach(func(string) (*App, error) { return nil, errors.New("no other environment") })
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range reach {
		paths = append(paths, r.Path)
	}
	want := []string{e.dev, e.tools, filepath.Join(e.dataDir, "home")}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Errorf("reach %q, want %q", paths, want)
	}
}

// A container whose inspect fails is not taken for absent unless the
// engine says so: a running sandbox's mounts are never left out of the
// exposed check because one call failed.
func TestSandboxMountsDocker(t *testing.T) {
	dir := t.TempDir()
	script := func(name, version, ls string) *backend.Docker {
		p := filepath.Join(dir, name)
		ckWriteFile(t, p, "#!/bin/sh\ncase \"$1\" in\n"+
			"inspect) echo 'Error: timed out' >&2; exit 1;;\n"+
			"version) exit "+version+";;\n"+
			"container) "+ls+";;\n"+
			"esac\nexit 2\n")
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return backend.NewDocker(&docker.CLI{Path: p, Stderr: io.Discard}, "caboose-x")
	}
	for _, tc := range []struct {
		name, version, ls string
		fail              bool
	}{
		{"listed", "0", "echo 0123abcd", true},
		{"none", "0", "exit 0", false},
		{"ls fails", "0", "exit 1", true},
		{"engine down", "1", "echo 0123abcd", false},
	} {
		ms, err := sandboxMounts(script(strings.ReplaceAll(tc.name, " ", "-"), tc.version, tc.ls))
		if (err != nil) != tc.fail || ms != nil {
			t.Errorf("%s: %v %v, want failure %v", tc.name, ms, err, tc.fail)
		}
	}
	// Not a container: its State decides.
	if ms, err := sandboxMounts(&backendtest.Fake{}); err != nil || ms != nil {
		t.Errorf("absent fake: %v %v", ms, err)
	}
	if ms, err := sandboxMounts(errMounts{&backendtest.Fake{Status: "running"}}); err == nil {
		t.Errorf("running fake: %v, want an error", ms)
	}
}

// When what no checkpoint may copy cannot be worked out, nothing is
// resolved: failing closed.
func TestCheckpointSourcesGuardFails(t *testing.T) {
	e := newCkEnv(t)
	e.app.Cfg.CabooseHome = ""
	got, why := e.resolve(t, "/work/dev/proj", "~/.claude")
	for _, p := range []string{"/work/dev/proj", "~/.claude"} {
		wantUnresolved(t, got, why, p, "CABOOSE_HOME is not set")
	}
}

func TestCheckpointSourcesLogin(t *testing.T) {
	e := newCkEnv(t)
	ckWriteFile(t, filepath.Join(e.dataDir, "home", ".claude", ".credentials.json"), "{}")
	got, why := e.resolve(t, "~/.claude/.credentials.json")
	wantUnresolved(t, got, why, "~/.claude/.credentials.json", "login")
}

// A root whose host path is a second name of a directory inside the store
// (a symlink, a /proc/self/fd magic link) is refused as a source, by
// identity, and makes the store exposed. (A bind mount, which
// needs privileges, is not covered.)
func TestCheckpointSourcesRefuseAliasesInsideTheStore(t *testing.T) {
	e := newCkEnv(t)
	inner := filepath.Join(checkpoint.StoreDir(e.envDir), "k-0000aaaa", "sub")
	ckMkdirs(t, inner)
	alias := filepath.Join(e.tmp, "alias")
	if err := os.Symlink(inner, alias); err != nil {
		t.Fatal(err)
	}
	hosts := map[string]string{"/work/a": alias}
	if f, err := os.Open(inner); err == nil {
		defer f.Close()
		if _, err := os.Stat("/proc/self/fd"); err == nil {
			hosts["/work/c"] = "/proc/self/fd/" + strconv.Itoa(int(f.Fd()))
		}
	}
	base := e.app.Cfg.Roots
	for ctr, host := range hosts {
		t.Run(ctr, func(t *testing.T) {
			e.app.Cfg.Roots = append(slices.Clone(base), config.Root{Name: "x", Host: host, Container: ctr, Writable: []string{"."}})
			got, why := e.resolve(t, ctr)
			wantUnresolved(t, got, why, ctr, "is inside the checkpoint store")
			s, err := e.app.openCheckpointStore(noOtherEnvs(t))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.Exposed(); !errors.Is(err, checkpoint.ErrExposed) {
				t.Errorf("root at %s: %v, want ErrExposed", host, err)
			}
		})
	}
}

// What the sandboxes reach is asked only of environments with a directory
// under CABOOSE_HOME: with a CABOOSE_HOME holding env x alone, the engine
// is never asked about caboose-default, the container of another
// CABOOSE_HOME's default environment. One that has a directory is asked.
func TestCheckpointReachAsksOnlyEnvsWithDir(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "caboose")
	envDir := config.EnvDirFor(home, "x")
	dataDir := filepath.Join(envDir, "data")
	ckMkdirs(t, filepath.Join(dataDir, "home"))
	log := filepath.Join(tmp, "docker.log")
	script := filepath.Join(tmp, "docker")
	ckWriteFile(t, script, "#!/bin/sh\necho \"$*\" >> '"+log+"'\ncase \"$1\" in\n"+
		"inspect) echo 'Error: No such object' >&2; exit 1;;\n"+
		"version) exit 0;;\n"+
		"container) exit 0;;\n"+
		"esac\nexit 2\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		switch k {
		case "CABOOSE_HOME":
			return home
		case "HOME":
			return tmp
		}
		return ""
	}
	a := &App{Cfg: &config.Config{Env: "x", CabooseHome: home, EnvDir: envDir, DataDir: dataDir,
		Container: config.ContainerFor("x"), Home: tmp, Getenv: getenv},
		Backend: &backendtest.Fake{}, Docker: &docker.CLI{Path: script, Stderr: io.Discard}, Stdout: io.Discard, Stderr: io.Discard}
	calls := func() string {
		b, err := os.ReadFile(log)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return string(b)
	}
	if _, err := a.checkpointReach(nil); err != nil {
		t.Fatal(err)
	}
	if _, notes := a.conservativePaths(nil); len(notes) != 0 {
		t.Errorf("notes %q", notes)
	}
	if c := calls(); strings.Contains(c, "caboose-default") {
		t.Errorf("the engine was asked about caboose-default:\n%s", c)
	}

	// Environment y has a directory: its sandbox is asked, both ways.
	ckMkdirs(t, config.EnvDirFor(home, "y"))
	if _, err := a.checkpointReach(nil); err != nil {
		t.Fatal(err)
	}
	if c := calls(); !strings.Contains(c, "caboose-y") || strings.Contains(c, "caboose-default") {
		t.Errorf("reach: docker calls\n%s\nwant caboose-y and no caboose-default", c)
	}
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	a.conservativePaths(nil)
	if c := calls(); !strings.Contains(c, "caboose-y") || strings.Contains(c, "caboose-default") {
		t.Errorf("conservative: docker calls\n%s\nwant caboose-y and no caboose-default", c)
	}
}
