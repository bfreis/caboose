package launcher

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/appbundle"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/backend/backendtest"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/saferm"
)

// A release runs the daemon in CABOOSE_HOME/daemon, a checkout's build in
// CABOOSE_HOME/daemon-dev.
func TestDaemonDir(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(e *updateEnv)
		want  string
	}{
		"managed release": {func(*updateEnv) {}, daemon.DirName},
		"other release":   {func(e *updateEnv) { e.a.Executable = func() (string, error) { return "/usr/local/bin/caboose", nil } }, daemon.DirName},
		"dev build":       {func(e *updateEnv) { e.version("v1.0.0-3-gabcdef0") }, daemon.DevDirName},
		"a checkout":      {func(e *updateEnv) { e.a.Checkout = "/src/caboose" }, daemon.DevDirName},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdateEnv(t)
			tc.setup(e)
			if got, want := e.a.daemonDir(), filepath.Join(e.a.Cfg.CabooseHome, tc.want); got != want {
				t.Errorf("daemon dir %s, want %s", got, want)
			}
		})
	}
}

// Starting the daemon runs `caboose daemon` from this executable.
func TestSpawnDaemon(t *testing.T) {
	e := newUpdateEnv(t)
	if err := e.a.daemonConfig().Spawn(); err != nil {
		t.Fatal(err)
	}
	if len(e.spawned) != 1 || !slices.Equal(e.spawned[0][1:], []string{"daemon"}) {
		t.Errorf("spawned %v", e.spawned)
	}
}

// Without the tests' Spawn, the daemon is started detached with exactly
// the minimal environment: nothing else of this process's.
func TestSpawnDaemonEnvironment(t *testing.T) {
	vars := map[string]string{"HOME": "/h", "LANG": "C", "SSH_AUTH_SOCK": "/s", "DOCKER_HOST": "unix:///d"}
	a := &App{Cfg: &config.Config{Home: "/h", CabooseHome: "/h/.caboose", Getenv: func(k string) string { return vars[k] }},
		Executable: func() (string, error) { return "/bin/caboose", nil }}
	old := startDetached
	t.Cleanup(func() { startDetached = old })
	var gotExe string
	var gotArgs, gotEnv []string
	startDetached = func(exe string, args, env []string) (*exec.Cmd, error) {
		gotExe, gotArgs, gotEnv = exe, args, env
		return nil, errors.New("not started")
	}
	if err := a.spawnDaemon(); err == nil {
		t.Fatal("no error from the stub")
	}
	want := []string{"HOME=/h", "LANG=C", "CABOOSE_HOME=/h/.caboose"}
	if gotExe != "/bin/caboose" || !slices.Equal(gotArgs, []string{"daemon"}) || !slices.Equal(gotEnv, want) {
		t.Errorf("started %s %v with %v, want env %v", gotExe, gotArgs, gotEnv, want)
	}
}

// status and stop with no daemon say so and create nothing, not even
// CABOOSE_HOME.
func TestDaemonStatusCreatesNothing(t *testing.T) {
	tmp, err := os.MkdirTemp("", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := saferm.RemoveRoot(tmp); err != nil {
			t.Error(err)
		}
	})
	home := filepath.Join(tmp, "ch")
	for _, arg := range []string{"status", "stop"} {
		out := &bytes.Buffer{}
		a := &App{Cfg: &config.Config{Home: tmp, CabooseHome: home, Getenv: func(string) string { return "" }},
			Stdout: out, Stderr: &bytes.Buffer{},
			Spawn: func(string, ...string) error { t.Error("spawned a daemon"); return nil }}
		if err := a.Daemon([]string{arg}); err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if !strings.Contains(out.String(), "not running") {
			t.Errorf("%s said %q", arg, out)
		}
	}
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Errorf("created %v", ents)
	}
}

// No daemon directory is ever reachable from a sandbox: no mount
// createContainer makes, nor any directory the vm backend shares for them,
// is in, or holds, CABOOSE_HOME/daemon or CABOOSE_HOME/daemon-dev (the
// daemon's socket and log in each), the broker's files, the jobs'
// log (EnvDir/jobs.log), or the environment's checkpoint store, EnvDir/checkpoints.
func TestDaemonDirsAreInNoMount(t *testing.T) {
	for _, iso := range isolations {
		t.Run(iso, func(t *testing.T) {
			b := newBoxApp(t, iso, &backendtest.Fake{})
			b.Cfg.CabooseHome = b.tmp
			root := filepath.Join(b.tmp, "dev")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			b.Cfg.Roots = []config.Root{{Name: "dev", Host: root, Container: "/work/dev"}}
			// The daemon's log, in each daemon directory.
			var tokens []string
			for _, d := range daemon.Dirs(b.tmp) {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
				tok := filepath.Join(d, daemon.LogFile)
				if err := os.WriteFile(tok, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				tokens = append(tokens, tok)
			}
			// The request broker's files, in the environment's dir: beside
			// the data dir the sandbox's home is mounted from (a mount of
			// the dir itself, or of anything above it, would hold them).
			envDir := config.EnvDirFor(b.tmp, b.Cfg.Env)
			if err := os.MkdirAll(envDir, 0o700); err != nil {
				t.Fatal(err)
			}
			private := brokerPrivatePaths(envDir)
			for _, f := range private {
				if err := os.WriteFile(f, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The daemon's stop and restart jobs' log, beside them.
			jobsLog := filepath.Join(envDir, jobsLogFile)
			if err := os.WriteFile(jobsLog, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			private = append(private, jobsLog, jobsLog+".1")
			// The environment's checkpoint store, beside the data dir too.
			store := checkpoint.StoreDir(envDir)
			if err := os.MkdirAll(filepath.Join(store, "k", "20261018T120000Z"), 0o700); err != nil {
				t.Fatal(err)
			}
			private = append(private, store)
			// make app's bundle, which the host runs, at every login once
			// it is a login item.
			bundleDir := filepath.Dir(appbundle.CheckoutPath(b.tmp))
			if err := os.MkdirAll(filepath.Join(appbundle.CheckoutPath(b.tmp), "Contents", "MacOS"), 0o755); err != nil {
				t.Fatal(err)
			}
			private = append(private, bundleDir)
			private = append(private, tokens...)
			if err := b.createContainer(false); err != nil {
				t.Fatalf("createContainer: %v\n%s", err, b.errb)
			}
			spec, _ := b.box.Spec()
			var sources []string
			for _, m := range spec.Mounts {
				sources = append(sources, filepath.Clean(m.Source))
			}
			// A VM shares each mount's source, or a file's directory,
			// and everything under it is the guest's to reach.
			shares, err := backend.PlanShares(spec.Mounts)
			if err != nil {
				t.Errorf("the vm backend's shares: %v", err)
			}
			for _, s := range shares {
				sources = append(sources, s.Path)
			}
			for _, src := range sources {
				for _, d := range daemon.Dirs(b.tmp) {
					if config.Within(src, d) || config.Within(d, src) {
						t.Errorf("%s is mounted or shared, and reaches the daemon directory %s", src, d)
					}
				}
				for _, f := range private {
					if config.Within(src, f) || config.Within(f, src) {
						t.Errorf("%s is mounted or shared, and reaches %s, which the request broker, the jobs' log, the checkpoint store, the web page's token or the menu-bar app's bundle keeps", src, f)
					}
				}
			}
		})
	}
}

// brokerPrivatePaths are what the request broker keeps in an
// environment's dir, none of which any sandbox may reach: its grants and
// requests, its log and the lock.
func brokerPrivatePaths(envDir string) []string {
	var out []string
	for _, n := range []string{"grants.json", "requests.json", "decisions.log", "decisions.log.1", "broker.lock"} {
		out = append(out, filepath.Join(envDir, n))
	}
	return out
}
