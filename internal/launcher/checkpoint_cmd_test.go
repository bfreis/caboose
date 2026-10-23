package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemonproto"
)

// ckDaemon is a daemon that answers writable, and status as fakeDaemon
// does.
type ckDaemon struct {
	fakeDaemon
	w     *daemonproto.WritableResult
	err   error
	calls int
}

func (d *ckDaemon) Writable(context.Context) (*daemonproto.WritableResult, error) {
	d.calls++
	if d.err != nil {
		return nil, d.err
	}
	return d.w, nil
}

// ckCLI is a ckEnv whose App prints to buffers and asks d for the
// daemon (nil: no daemon answers).
func ckCLI(t *testing.T, d *ckDaemon, paths ...string) (*ckEnv, *bytes.Buffer) {
	t.Helper()
	e := newCkEnv(t, paths...)
	var out bytes.Buffer
	e.app.Stdout, e.app.Stderr = &out, &out
	e.app.DaemonDial = func(context.Context, bool) (DaemonCaller, error) {
		if d == nil {
			return nil, daemonproto.NewError(daemonproto.KindUnavailable, "no daemon answers")
		}
		return d, nil
	}
	return e, &out
}

// noOtherEnvs makes another environment's App: none exists in these
// tests, so asking for one is a test failure.
func noOtherEnvs(t *testing.T) func(string) (*App, error) {
	return func(env string) (*App, error) {
		t.Errorf("asked for environment %s's App", env)
		return nil, errors.New("no such environment")
	}
}

func (e *ckEnv) run(t *testing.T, o checkpointOpts, args ...string) error {
	t.Helper()
	if o.apps == nil {
		o.apps = noOtherEnvs(t)
	}
	return e.app.checkpointCmd(args, o)
}

// ckIDs are the IDs of the store's checkpoints of key, newest first.
func (e *ckEnv) ckIDs(t *testing.T, key string) []string {
	t.Helper()
	if _, err := os.Lstat(checkpoint.StoreDir(e.envDir)); err != nil {
		return nil
	}
	s, err := checkpoint.Open(e.home, e.envDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	es, err := s.List(key)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range es {
		ids = append(ids, x.ID.String())
	}
	return ids
}

// clock is a clock that moves a minute at each reading.
func clock() func() time.Time {
	t := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Minute)
		return t
	}
}

func TestCheckpointCreate(t *testing.T) {
	e, out := ckCLI(t, nil, "/work/dev/proj", "/work/tools", "~/.claude", "/work/dev/proj2")
	ckWriteFile(t, filepath.Join(e.dev, "proj", "a.txt"), "a")
	key := checkpoint.Key("/work/dev/proj")
	o := checkpointOpts{now: clock()}

	// A path not configured is refused before anything is taken.
	if err := e.run(t, o, "create", "/work/dev/proj", "/work/dev/other"); err == nil || !strings.Contains(err.Error(), "add it to [checkpoints] paths first") {
		t.Fatalf("an unconfigured path: %v", err)
	}
	if ids := e.ckIDs(t, key); len(ids) != 0 {
		t.Fatalf("taken anyway: %v", ids)
	}

	// Named, by either spelling of the home.
	out.Reset()
	if err := e.run(t, o, "create", "/work/dev/proj", "/home/agent/.claude"); err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	if ids := e.ckIDs(t, key); len(ids) != 1 || !strings.Contains(out.String(), "/work/dev/proj: took "+ids[0]) {
		t.Fatalf("ids %v, output:\n%s", ids, out)
	}
	if ids := e.ckIDs(t, checkpoint.Key("~/.claude")); len(ids) != 1 {
		t.Fatalf("~/.claude: %v", ids)
	}
	if ids := e.ckIDs(t, checkpoint.Key("/work/tools")); len(ids) != 0 {
		t.Fatalf("a path not named was taken: %v", ids)
	}

	// Every configured path; one that does not resolve says why and
	// fails the command, the others are taken.
	out.Reset()
	err := e.run(t, o, "create")
	if err == nil || !strings.Contains(err.Error(), "1 of 4") || !strings.Contains(out.String(), "/work/dev/proj2: not taken:") {
		t.Fatalf("create all: %v\n%s", err, out)
	}
	if ids := e.ckIDs(t, key); len(ids) != 2 {
		t.Fatalf("proj: %v", ids)
	}
	if ids := e.ckIDs(t, checkpoint.Key("/work/tools")); len(ids) != 1 {
		t.Fatalf("tools: %v", ids)
	}
	s, err := checkpoint.Open(e.home, e.envDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Resolve(checkpoint.ID{Key: key, Latest: true})
	if err != nil || got.Meta == nil || got.Meta.Reason != checkpoint.ReasonManual {
		t.Fatalf("latest: %+v, %v", got, err)
	}

	// None configured.
	e.app.Cfg.Checkpoints = nil
	if err := e.run(t, o, "create"); err == nil || !strings.Contains(err.Error(), "no [checkpoints] paths") {
		t.Fatalf("none configured: %v", err)
	}
}

// create takes the store's lock, as the daemon's schedule does: while
// another holds it, nothing is taken.
func TestCheckpointCreateUnderTheLock(t *testing.T) {
	e, out := ckCLI(t, nil, "/work/dev/proj")
	key := checkpoint.Key("/work/dev/proj")
	s, err := checkpoint.Open(e.home, e.envDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	l, err := s.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	o := checkpointOpts{now: clock(), lockWait: 300 * time.Millisecond}
	err = e.run(t, o, "create")
	if !errors.Is(err, checkpoint.ErrLocked) && (err == nil || !strings.Contains(err.Error(), "a checkpoint is being taken")) {
		t.Fatalf("with the lock held elsewhere: %v\n%s", err, out)
	}
	if ids := e.ckIDs(t, key); len(ids) != 0 {
		t.Fatalf("taken without the lock: %v", ids)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := e.run(t, o, "create"); err != nil {
		t.Fatalf("after the lock is free: %v\n%s", err, out)
	}
	if ids := e.ckIDs(t, key); len(ids) != 1 {
		t.Fatalf("after: %v", ids)
	}
}

func TestCheckpointList(t *testing.T) {
	d := &ckDaemon{}
	e, out := ckCLI(t, d, "/work/dev/proj", "/work/dev/missing")
	key := checkpoint.Key("/work/dev/proj")
	o := checkpointOpts{now: clock()}
	if err := e.run(t, o, "create", "/work/dev/proj"); err != nil {
		t.Fatal(err)
	}
	id := e.ckIDs(t, key)[0]
	// A key of a path no longer listed.
	e.app.Cfg.Checkpoints = []string{"/work/tools"}
	if err := e.run(t, o, "create"); err != nil {
		t.Fatal(err)
	}
	e.app.Cfg.Checkpoints = []string{"/work/dev/proj", "/work/dev/missing"}

	next := time.Now().Add(4*time.Minute + 30*time.Second)
	d.Stat = &daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{{Name: config.DefaultEnv, Checkpoints: &daemonproto.CheckpointStatus{
		Served: "held", Paths: []daemonproto.CheckpointPathStatus{{Path: "/work/dev/proj", Key: key, NextAt: next}}}}}}
	out.Reset()
	if err := e.run(t, checkpointOpts{}, "list"); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"/work/dev/proj  (key " + key + "): copied from this machine",
		"next in 4 min",
		id + "  ", "manual", "(latest)",
		"/work/dev/missing  (key " + checkpoint.Key("/work/dev/missing") + "): not copied now:", "not a writable part of a root",
		"no longer in [checkpoints] paths", "/work/tools  (key " + checkpoint.Key("/work/tools") + ")",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, text)
		}
	}

	// --json is the daemon's checkpoints listing, filtered by a key or a
	// path.
	for _, f := range []string{key, "/work/dev/proj"} {
		out.Reset()
		if err := e.run(t, checkpointOpts{}, "list", f, "--json"); err != nil {
			t.Fatal(err)
		}
		var r daemonproto.CheckpointsResult
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("%v:\n%s", err, out)
		}
		if r.Validate() != nil || len(r.Paths) != 1 || len(r.Orphans) != 0 || r.Paths[0].Checkpoints[0].ID != id || !r.Paths[0].Resolved {
			t.Fatalf("%s: %+v", f, r)
		}
	}
	if err := e.run(t, checkpointOpts{}, "list", "nope-00000000"); err == nil {
		t.Fatal("an unknown key was listed")
	}

	// Without the daemon, the store all the same.
	d.Stat = nil
	out.Reset()
	if err := e.run(t, checkpointOpts{}, "list"); err != nil || !strings.Contains(out.String(), "daemon does not answer") || !strings.Contains(out.String(), id) {
		t.Fatalf("%v\n%s", err, out)
	}
}

// restoreEnv is a ckEnv with one checkpoint of /work/dev/proj, holding a
// repository whose config runs a command and that has a hook.
func restoreEnv(t *testing.T, d *ckDaemon) (*ckEnv, *bytes.Buffer, string) {
	t.Helper()
	e, out := ckCLI(t, d, "/work/dev/proj")
	proj := filepath.Join(e.dev, "proj")
	ckWriteFile(t, filepath.Join(proj, "a.txt"), "a")
	ckWriteFile(t, filepath.Join(proj, ".git", "HEAD"), "ref: refs/heads/main\n")
	ckMkdirs(t, filepath.Join(proj, ".git", "objects"))
	ckWriteFile(t, filepath.Join(proj, ".git", "config"), "[core]\n\tfsmonitor = /bin/false\n")
	ckWriteFile(t, filepath.Join(proj, ".git", "hooks", "pre-commit"), "#!/bin/sh\n")
	if err := e.run(t, checkpointOpts{now: clock()}, "create"); err != nil {
		t.Fatal(err)
	}
	ids := e.ckIDs(t, checkpoint.Key("/work/dev/proj"))
	if len(ids) != 1 {
		t.Fatalf("checkpoints %v", ids)
	}
	return e, out, ids[0]
}

func TestCheckpointRestore(t *testing.T) {
	d := &ckDaemon{w: &daemonproto.WritableResult{}}
	e, out, id := restoreEnv(t, d)
	key := checkpoint.Key("/work/dev/proj")
	cmp := filepath.Join(e.tmp, "cmp")
	ckMkdirs(t, cmp)
	d.w.ReadOnly = []daemonproto.WritablePath{{Path: phys(t, cmp), Envs: []string{"beta"}}}
	to := filepath.Join(cmp, "restored")
	out.Reset()
	if err := e.run(t, checkpointOpts{now: clock()}, "restore", id, "--to", to); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	text := out.String()
	ids := e.ckIDs(t, key)
	if len(ids) != 2 || ids[1] != id {
		t.Fatalf("checkpoints after: %v", ids)
	}
	for _, want := range []string{
		"First took " + ids[0] + ", a checkpoint of /work/dev/proj",
		"Restored " + id + " (/work/dev/proj) to " + phys(t, cmp) + "/restored",
		"The sandboxes of beta can read it",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Warning") || strings.Contains(text, "could not say") {
		t.Errorf("warned:\n%s", text)
	}
	if b, err := os.ReadFile(filepath.Join(to, "a.txt")); err != nil || string(b) != "a" {
		t.Fatalf("restored a.txt: %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(to, ".git", "config")); err != nil || !strings.Contains(string(b), "fsmonitor") {
		t.Fatalf("restored config: %q, %v", b, err)
	}
	if d.calls != 1 {
		t.Fatalf("writable asked %d times", d.calls)
	}

	// Into what exists: refused, nothing written, no checkpoint taken.
	for name, mk := range map[string]func(string){
		"a file":             func(p string) { ckWriteFile(t, p, "keep") },
		"an empty dir":       func(p string) { ckMkdirs(t, p) },
		"a dangling symlink": func(p string) { _ = os.Symlink(filepath.Join(e.tmp, "nowhere"), p) },
	} {
		p := filepath.Join(e.tmp, "exists-"+strings.ReplaceAll(name, " ", "-"))
		mk(p)
		before, _ := os.Lstat(p)
		out.Reset()
		err := e.run(t, checkpointOpts{now: clock()}, "restore", key+"@latest", "--to="+p)
		if err == nil || !strings.Contains(err.Error(), "restore writes only into a new directory") {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		after, _ := os.Lstat(p)
		if !os.SameFile(before, after) || after.ModTime() != before.ModTime() || after.Size() != before.Size() {
			t.Fatalf("%s changed", name)
		}
		if b, err := os.ReadFile(p); name == "a file" && (err != nil || string(b) != "keep") {
			t.Fatalf("%s: %q", name, b)
		}
		if got := e.ckIDs(t, key); len(got) != 2 {
			t.Fatalf("%s: a checkpoint was taken: %v", name, got)
		}
	}
	entries, _ := os.ReadDir(e.tmp)
	for _, x := range entries {
		if strings.HasPrefix(x.Name(), ".caboose-restore-") {
			t.Fatalf("left %s", x.Name())
		}
	}

	// Bad command lines.
	for _, args := range [][]string{{"restore", id}, {"restore", "--to", to}, {"restore", "../x", "--to", to + "2"}, {"restore", id, "--to"}, {"restore", id, id, "--to", to + "3"}} {
		if err := e.run(t, checkpointOpts{}, args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

// A target a sandbox can write is refused, naming the environments whose
// sandboxes can, as the daemon says them; so is one a configured root
// would hand a sandbox at its next start, mounted or not. A read-only
// root's place is allowed, its readers said.
func TestCheckpointRestoreIntoWritable(t *testing.T) {
	d := &ckDaemon{w: &daemonproto.WritableResult{}}
	e, out, id := restoreEnv(t, d)
	w := filepath.Join(e.tmp, "shared")
	ckMkdirs(t, w, filepath.Join(e.tools, "x"))
	d.w.Writable = []daemonproto.WritablePath{{Path: phys(t, w), Envs: []string{"beta", "gamma"}}}
	for to, want := range map[string]string{
		filepath.Join(w, "r"):                  "the sandboxes of environments beta, gamma can write",
		filepath.Join(e.dev, "proj", "r"):      "the sandboxes of environment default can write",
		filepath.Join(e.tools, "x", "r"):       "the sandboxes of environment default can write",
		filepath.Join(e.home, "restored-here"): "is inside CABOOSE_HOME",
	} {
		out.Reset()
		err := e.run(t, checkpointOpts{now: clock()}, "restore", id, "--to", to)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v\n%s", to, err, out)
		}
		if _, err := os.Lstat(to); err == nil {
			t.Fatalf("%s written", to)
		}
		if strings.Contains(out.String(), "could not say") {
			t.Fatalf("the daemon's answer was not used:\n%s", out)
		}
	}
	out.Reset()
	to := filepath.Join(e.dev, "other", "r")
	if err := e.run(t, checkpointOpts{now: clock()}, "restore", id, "--to", to); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "The sandboxes of default can read it") {
		t.Fatalf("readers not said:\n%s", out)
	}
}

// configuredPaths counts every environment's configured roots, sandbox or
// not: writable parts and wholly writable roots as writable, the rest
// read-only; an environment it cannot read is a note.
func TestConfiguredPaths(t *testing.T) {
	e, _ := ckCLI(t, nil)
	other := filepath.Join(e.tmp, "other")
	ckMkdirs(t, filepath.Join(other, "part"), config.EnvDirFor(e.home, "beta"), config.EnvDirFor(e.home, "broken"))
	apps := func(env string) (*App, error) {
		switch env {
		case "beta":
			return &App{Cfg: &config.Config{Env: "beta", CabooseHome: e.home,
				Roots: []config.Root{{Host: other, Container: "/work/o", Writable: []string{"part"}}}}, Backend: &backendtest.Fake{}}, nil
		case "broken":
			return nil, errors.New("config.toml: bad")
		}
		t.Errorf("asked for %s", env)
		return nil, errors.New("no")
	}
	set := checkpoint.PathSet{Writable: []checkpoint.EnvPath{{Env: "default", Path: phys(t, e.tools)}}}
	notes := e.app.configuredPaths(apps, &set)
	wantW := []checkpoint.EnvPath{{Env: "default", Path: phys(t, e.tools)}, {Env: "default", Path: phys(t, filepath.Join(e.dev, "proj"))},
		{Env: "beta", Path: phys(t, filepath.Join(other, "part"))}}
	wantR := []checkpoint.EnvPath{{Env: "default", Path: phys(t, e.dev)}, {Env: "beta", Path: phys(t, other)}}
	slices.SortFunc(wantW, cmpEnvPath)
	slices.SortFunc(wantR, cmpEnvPath)
	slices.SortFunc(set.Writable, cmpEnvPath)
	slices.SortFunc(set.ReadOnly, cmpEnvPath)
	if !slices.Equal(set.Writable, wantW) || !slices.Equal(set.ReadOnly, wantR) || set.Fallback {
		t.Fatalf("%+v", set)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "broken") {
		t.Fatalf("notes %q", notes)
	}
}

func cmpEnvPath(a, b checkpoint.EnvPath) int {
	return strings.Compare(a.Env+"\x00"+a.Path, b.Env+"\x00"+b.Path)
}

// Without the daemon, every root of every environment, as configured and
// as mounted, and every mount and data dir home, count as writable, and
// the restore says so and refuses.
func TestCheckpointRestoreWithoutTheDaemon(t *testing.T) {
	for name, d := range map[string]*ckDaemon{
		"none answers":      nil,
		"writable fails":    {err: daemonproto.NewError(daemonproto.KindUnknownMethod, "an older daemon")},
		"an invalid answer": {w: &daemonproto.WritableResult{Writable: []daemonproto.WritablePath{{Path: "relative", Envs: []string{"a"}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			e, out, id := restoreEnv(t, d)
			ckMkdirs(t, filepath.Join(e.dev, "other", "x"))
			out.Reset()
			if d != nil && d.err != nil {
				d.err = daemonproto.NewError(daemonproto.KindUnknownMethod, "older")
				d.err.(*daemonproto.Error).Message = "an older daemon\x1b[2J"
			}
			to := filepath.Join(e.dev, "other", "x", "r")
			err := e.run(t, checkpointOpts{now: clock()}, "restore", id, "--to", to)
			if err == nil || !strings.Contains(err.Error(), "the sandboxes of environment default can write") {
				t.Fatalf("%v\n%s", err, out)
			}
			if _, err := os.Lstat(to); err == nil {
				t.Fatal("written")
			}
			if strings.Contains(out.String(), "\x1b") {
				t.Errorf("a control character printed:\n%q", out)
			}
			for _, want := range []string{"The daemon could not say what the sandboxes write", "counted every root of every environment"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("no %q in:\n%s", want, out)
				}
			}
		})
	}
}

// The conservative set holds every root of every environment, as
// configured and as its sandbox mounts it, every mount, and every data
// dir's home: never fewer.
func TestConservativePaths(t *testing.T) {
	e := newCkEnv(t)
	other := filepath.Join(e.tmp, "other")
	mounted := filepath.Join(e.tmp, "mounted")
	extra := filepath.Join(e.tmp, "extra")
	ckMkdirs(t, other, mounted, extra, config.EnvDirFor(e.home, "beta"), config.EnvDirFor(e.home, "broken"))
	e.box.Status = "running"
	e.box.SandboxLabels = map[string]string{assets.LabelRoots: `["/work/m"]`, assets.LabelWritable: `{"/work/m":["p"]}`}
	e.box.SandboxMounts = []backend.Mount{{Source: mounted, Target: "/work/m"}, {Source: extra, Target: "/home/agent/.npm"}}
	betaBox := &backendtest.Fake{Status: "running", SandboxMounts: []backend.Mount{{Source: filepath.Join(e.tmp, "betamount"), Target: "/x"}}}
	betaData := filepath.Join(e.tmp, "betadata")
	apps := func(env string) (*App, error) {
		switch env {
		case "beta":
			return &App{Cfg: &config.Config{Env: "beta", CabooseHome: e.home, DataDir: betaData,
				Roots: []config.Root{{Host: other, Container: "/work/o", Writable: []string{}}}}, Backend: betaBox}, nil
		case "broken":
			return nil, errors.New("config.toml: bad")
		}
		t.Errorf("asked for %s", env)
		return nil, errors.New("no")
	}
	set, notes := e.app.conservativePaths(apps)
	if !set.Fallback || len(set.ReadOnly) != 0 {
		t.Fatalf("%+v", set)
	}
	for _, want := range []checkpoint.EnvPath{
		{Env: "default", Path: phys(t, e.dev)}, {Env: "default", Path: phys(t, e.tools)},
		{Env: "default", Path: phys(t, mounted)}, {Env: "default", Path: phys(t, extra)},
		{Env: "default", Path: phys(t, filepath.Join(e.dataDir, "home"))},
		{Env: "beta", Path: phys(t, other)}, {Env: "beta", Path: filepath.Join(e.tmp, "betamount")},
		{Env: "beta", Path: filepath.Join(betaData, "home")},
	} {
		if !slices.Contains(set.Writable, want) {
			t.Errorf("no %+v in %+v", want, set.Writable)
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "broken") {
		t.Fatalf("notes %q", notes)
	}
}

// writable is read off the sandboxes when asked: what each can write, and
// the roots each mounts read-only, never an earlier answer.
func TestDaemonWritable(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "caboose")
	dev, proj, claude := filepath.Join(tmp, "dev"), filepath.Join(tmp, "dev", "proj"), filepath.Join(tmp, "claude")
	ckMkdirs(t, proj, claude, config.EnvDirFor(home, "alpha"), config.EnvDirFor(home, "beta"))
	alpha := &backendtest.Fake{Status: "running",
		SandboxLabels: map[string]string{assets.LabelRoots: `["/work/dev"]`, assets.LabelWritable: `{"/work/dev":["proj"]}`},
		SandboxMounts: []backend.Mount{{Source: dev, Target: "/work/dev"}, {Source: proj, Target: "/work/dev/proj"}, {Source: claude, Target: "/home/agent/.claude"}}}
	beta := &backendtest.Fake{Status: "exited",
		SandboxLabels: map[string]string{assets.LabelRoots: `["/work/dev"]`},
		SandboxMounts: []backend.Mount{{Source: dev, Target: "/work/dev"}}}
	var boxErr error
	boxes := func(env string) (backend.Backend, bool, error) {
		switch env {
		case "alpha":
			return alpha, true, nil
		case "beta":
			return beta, true, boxErr
		}
		return nil, false, nil
	}
	l := NewDaemonLinks(DaemonLinksOptions{Dir: filepath.Join(home, "daemon"), CabooseHome: home, NewApp: func(env string) (*App, error) {
		return &App{Cfg: &config.Config{Env: env, CabooseHome: home, Getenv: func(string) string { return "" }}, envBox: boxes}, nil
	}})
	defer l.Close()

	r, err := l.Writable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	find := func(list []daemonproto.WritablePath, p string) []string {
		for _, x := range list {
			if x.Path == p {
				return x.Envs
			}
		}
		return nil
	}
	if got := find(r.Writable, phys(t, proj)); !slices.Equal(got, []string{"alpha"}) {
		t.Errorf("proj writable by %v; %+v", got, r)
	}
	if got := find(r.Writable, phys(t, claude)); !slices.Equal(got, []string{"alpha"}) {
		t.Errorf("claude writable by %v", got)
	}
	// beta's sandbox has no writable label: all of the root is its.
	if got := find(r.Writable, phys(t, dev)); !slices.Equal(got, []string{"beta"}) {
		t.Errorf("dev writable by %v", got)
	}
	if got := find(r.ReadOnly, phys(t, dev)); !slices.Equal(got, []string{"alpha"}) {
		t.Errorf("dev read-only for %v", got)
	}

	// A sandbox recreated with another mount: the next answer has it.
	newer := filepath.Join(tmp, "newer")
	ckMkdirs(t, newer)
	alpha.SandboxMounts = append(alpha.SandboxMounts, backend.Mount{Source: newer, Target: "/work/newer"})
	r, err = l.Writable(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := find(r.Writable, phys(t, newer)); !slices.Equal(got, []string{"alpha"}) {
		t.Errorf("a stale answer: %+v", r.Writable)
	}

	// A sandbox it cannot ask: no answer, rather than one without it.
	boxErr = errors.New("the engine does not answer")
	if _, err := l.Writable(t.Context()); err == nil {
		t.Fatal("answered without beta's sandbox")
	}
}

// With environments none of whose configurations loads, writable has no
// answer: never an empty one, which would say no sandbox writes anything.
func TestDaemonWritableNoConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "caboose")
	ckMkdirs(t, config.EnvDirFor(home, "alpha"))
	l := NewDaemonLinks(DaemonLinksOptions{Dir: filepath.Join(home, "daemon"), CabooseHome: home, NewApp: func(string) (*App, error) {
		return nil, errors.New("config.toml: bad")
	}})
	defer l.Close()
	if r, err := l.Writable(t.Context()); err == nil {
		t.Fatalf("answered %+v", r)
	}
	// No environment at all: nothing runs.
	empty := filepath.Join(t.TempDir(), "caboose")
	l2 := NewDaemonLinks(DaemonLinksOptions{Dir: filepath.Join(empty, "daemon"), CabooseHome: empty, NewApp: func(string) (*App, error) {
		return nil, errors.New("not asked")
	}})
	defer l2.Close()
	if r, err := l2.Writable(t.Context()); err != nil || len(r.Writable)+len(r.ReadOnly) != 0 {
		t.Fatalf("%+v, %v", r, err)
	}
}

// The App's daemon paths: the daemon's answer as restore's PathSet.
func TestDaemonPaths(t *testing.T) {
	d := &ckDaemon{w: &daemonproto.WritableResult{
		Writable: []daemonproto.WritablePath{{Path: "/w", Envs: []string{"a", "b"}}},
		ReadOnly: []daemonproto.WritablePath{{Path: "/r", Envs: []string{"c"}}}}}
	e, _ := ckCLI(t, d)
	set, err := e.app.daemonPaths(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := checkpoint.PathSet{Writable: []checkpoint.EnvPath{{Env: "a", Path: "/w"}, {Env: "b", Path: "/w"}}, ReadOnly: []checkpoint.EnvPath{{Env: "c", Path: "/r"}}}
	if !slices.Equal(set.Writable, want.Writable) || !slices.Equal(set.ReadOnly, want.ReadOnly) || set.Fallback {
		t.Fatalf("%+v", set)
	}
	// A daemon that cannot answer writable at all.
	e.app.DaemonDial = func(context.Context, bool) (DaemonCaller, error) { return &fakeDaemon{}, nil }
	if _, err := e.app.daemonPaths(t.Context()); err == nil {
		t.Fatal("no error from a daemon without writable")
	}
}

func TestCheckpointUsage(t *testing.T) {
	e, _ := ckCLI(t, nil)
	for _, args := range [][]string{{}, {"rm"}, {"list", "--all"}, {"list", "a", "b"}, {"create", "--force"}} {
		if err := e.run(t, checkpointOpts{}, args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

// Below the soft floor a manual create still takes its checkpoint, and
// says the scheduled ones are skipped; below the hard floor it takes none.
func TestCheckpointCreateLowSpace(t *testing.T) {
	e, out := ckCLI(t, nil, "/work/dev/proj")
	ckWriteFile(t, filepath.Join(e.dev, "proj", "a.txt"), "a")
	key := checkpoint.Key("/work/dev/proj")
	o := checkpointOpts{now: clock()}
	o.take.Statfs = func(string) (uint64, uint64, error) { return 20 << 30, 1000 << 30, nil }
	if err := e.run(t, o, "create"); err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	if ids := e.ckIDs(t, key); len(ids) != 1 || !strings.Contains(out.String(), "below its soft floor: taken, since you asked, but the scheduled ones are skipped") {
		t.Fatalf("ids %v, output:\n%s", ids, out)
	}
	out.Reset()
	o.take.Statfs = func(string) (uint64, uint64, error) { return 1 << 20, 1000 << 30, nil }
	if err := e.run(t, o, "create"); err == nil {
		t.Fatalf("taken below the hard floor:\n%s", out)
	}
	if ids := e.ckIDs(t, key); len(ids) != 1 {
		t.Fatalf("ids %v", ids)
	}
}
